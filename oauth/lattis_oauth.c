/*-------------------------------------------------------------------------
 *
 * lattis_oauth.c
 *	  Module de validation OAuth de PostgreSQL 18 pour le Serveur Lattis.
 *
 * PostgreSQL ne connaît qu'un émetteur : lattisd. Le module vérifie, sans
 * accès réseau ni lecture de table, les jetons d'accès que lattisd signe
 * (JWT EdDSA / Ed25519, typ « at+jwt ») :
 *   - signature, avec l'une des clés publiques du fichier
 *     lattis_oauth.public_keys_file (relu à chaque connexion : la rotation
 *     des clés ne demande pas de rechargement) ;
 *   - iss = lattis_oauth.issuer, aud contient lattis_oauth.audience,
 *     exp et nbf (tolérance lattis_oauth.leeway secondes) ;
 *   - scope contient lattis_oauth.required_scope ;
 *   - pg_role = rôle demandé à la connexion.
 * L'identité authentifiée (system_user) est « lattis:<sub> ».
 *
 * pg_hba.conf :
 *   hostssl all all 0.0.0.0/0 oauth issuer="https://lattisd/" scope="openid lattis"
 *           validator=lattis_oauth delegate_ident_mapping=1
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <time.h>

#include <openssl/bio.h>
#include <openssl/evp.h>
#include <openssl/pem.h>

#include "fmgr.h"
#include "libpq/oauth.h"
#include "nodes/miscnodes.h"
#include "storage/fd.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/jsonb.h"
#include "utils/numeric.h"

PG_MODULE_MAGIC_EXT(.name = "lattis_oauth", .version = "0.1.0");

#define MAX_KEYS 16
#define MAX_KEY_FILE (64 * 1024)
#define MAX_TOKEN (16 * 1024)

static char *issuer = NULL;
static char *public_keys_file = NULL;
static char *audience = NULL;
static char *required_scope = NULL;
static int	leeway = 60;

static bool validate_token(const ValidatorModuleState *state, const char *token,
						   const char *role, ValidatorModuleResult *result);

static const OAuthValidatorCallbacks callbacks = {
	PG_OAUTH_VALIDATOR_MAGIC,
	.validate_cb = validate_token,
};

void
_PG_init(void)
{
	DefineCustomStringVariable("lattis_oauth.issuer",
							   "Émetteur attendu (URL publique de lattisd).",
							   NULL, &issuer, "", PGC_SIGHUP, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("lattis_oauth.public_keys_file",
							   "Fichier PEM des clés publiques Ed25519 de lattisd.",
							   NULL, &public_keys_file, "", PGC_SIGHUP, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("lattis_oauth.audience",
							   "Audience attendue des jetons d'accès.",
							   NULL, &audience, "postgres", PGC_SIGHUP, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("lattis_oauth.required_scope",
							   "Scope exigé dans les jetons d'accès.",
							   NULL, &required_scope, "lattis", PGC_SIGHUP, 0, NULL, NULL, NULL);
	DefineCustomIntVariable("lattis_oauth.leeway",
							"Tolérance d'horloge, en secondes.",
							NULL, &leeway, 60, 0, 600, PGC_SIGHUP, GUC_UNIT_S, NULL, NULL, NULL);
	MarkGUCPrefixReserved("lattis_oauth");
}

const OAuthValidatorCallbacks *
_PG_oauth_validator_module_init(void)
{
	return &callbacks;
}

/* Refus journalisé : le client ne reçoit qu'un échec d'authentification. */
#define REJECT(...) \
	do { \
		ereport(LOG, (errmsg("lattis_oauth : jeton refusé : " __VA_ARGS__))); \
		return true; \
	} while (0)

/* Décode du base64url sans remplissage. Renvoie la longueur, ou -1. */
static int
b64url_decode(const char *in, int inlen, unsigned char *out)
{
	uint32		acc = 0;
	int			bits = 0;
	int			n = 0;

	for (int i = 0; i < inlen; i++)
	{
		char		c = in[i];
		int			v;

		if (c >= 'A' && c <= 'Z')
			v = c - 'A';
		else if (c >= 'a' && c <= 'z')
			v = c - 'a' + 26;
		else if (c >= '0' && c <= '9')
			v = c - '0' + 52;
		else if (c == '-')
			v = 62;
		else if (c == '_')
			v = 63;
		else
			return -1;
		acc = (acc << 6) | v;
		bits += 6;
		if (bits >= 8)
		{
			bits -= 8;
			out[n++] = (unsigned char) (acc >> bits);
		}
	}
	/* Bits de remplissage non nuls : encodage non canonique. */
	if (bits >= 6 || (acc & ((1u << bits) - 1)) != 0)
		return -1;
	return n;
}

/* Décode un segment base64url en chaîne terminée par NUL. */
static char *
decode_segment(const char *in, int inlen, int *outlen)
{
	unsigned char *out = palloc(inlen * 3 / 4 + 2);
	int			n = b64url_decode(in, inlen, out);

	if (n < 0)
		return NULL;
	out[n] = '\0';
	if (outlen)
		*outlen = n;
	return (char *) out;
}

/* Lit un objet JSON en jsonb sans lever d'erreur. */
static Jsonb *
parse_json(const char *text)
{
	ErrorSaveContext escontext = {T_ErrorSaveContext};
	Datum		d;

	if (!DirectInputFunctionCallSafe(jsonb_in, (char *) text, InvalidOid, -1,
									 (Node *) &escontext, &d))
		return NULL;
	return DatumGetJsonbP(d);
}

static JsonbValue *
get_key(Jsonb *jb, const char *key, JsonbValue *buf)
{
	if (!JB_ROOT_IS_OBJECT(jb))
		return NULL;
	return getKeyJsonValueFromContainer(&jb->root, key, strlen(key), buf);
}

/* Valeur texte (copiée, terminée par NUL) d'une clé, ou NULL. */
static char *
get_string(Jsonb *jb, const char *key)
{
	JsonbValue	buf;
	JsonbValue *v = get_key(jb, key, &buf);

	if (v == NULL || v->type != jbvString)
		return NULL;
	return pnstrdup(v->val.string.val, v->val.string.len);
}

/* Valeur entière d'une clé numérique ; false si absente ou invalide. */
static bool
get_int64(Jsonb *jb, const char *key, int64 *out)
{
	JsonbValue	buf;
	JsonbValue *v = get_key(jb, key, &buf);
	char	   *s;
	char	   *end;

	if (v == NULL || v->type != jbvNumeric)
		return false;
	s = DatumGetCString(DirectFunctionCall1(numeric_out, NumericGetDatum(v->val.numeric)));
	errno = 0;
	*out = strtoll(s, &end, 10);
	return errno == 0 && end != s && *end == '\0';
}

/* aud est une chaîne ou une liste de chaînes. */
static bool
audience_matches(Jsonb *jb, const char *want)
{
	JsonbValue	buf;
	JsonbValue *v = get_key(jb, "aud", &buf);

	if (v == NULL)
		return false;
	if (v->type == jbvString)
		return v->val.string.len == (int) strlen(want) &&
			memcmp(v->val.string.val, want, v->val.string.len) == 0;
	if (v->type == jbvBinary && JsonContainerIsArray(v->val.binary.data))
	{
		JsonbValue	target;

		target.type = jbvString;
		target.val.string.val = (char *) want;
		target.val.string.len = strlen(want);
		return findJsonbValueFromContainer(v->val.binary.data, JB_FARRAY, &target) != NULL;
	}
	return false;
}

static bool
scope_contains(const char *scope, const char *want)
{
	size_t		wl = strlen(want);
	const char *p = scope;

	while (*p)
	{
		const char *end = strchr(p, ' ');
		size_t		len = end ? (size_t) (end - p) : strlen(p);

		if (len == wl && strncmp(p, want, wl) == 0)
			return true;
		if (!end)
			break;
		p = end + 1;
	}
	return false;
}

/*
 * Vérifie la signature avec l'une des clés du fichier. Le fichier est relu à
 * chaque connexion : une rotation de clé ne demande ni redémarrage ni
 * rechargement.
 */
static bool
signature_valid(const char *input, size_t inputlen, const unsigned char *sig, size_t siglen)
{
	FILE	   *f;
	char	   *buf;
	size_t		n;
	BIO		   *bio;
	EVP_PKEY   *key;
	bool		ok = false;
	int			count = 0;

	if (public_keys_file == NULL || public_keys_file[0] == '\0')
	{
		ereport(LOG, (errmsg("lattis_oauth : lattis_oauth.public_keys_file n'est pas réglé")));
		return false;
	}
	f = AllocateFile(public_keys_file, "r");
	if (f == NULL)
	{
		ereport(LOG, (errcode_for_file_access(),
					  errmsg("lattis_oauth : lecture de « %s » impossible : %m", public_keys_file)));
		return false;
	}
	buf = palloc(MAX_KEY_FILE);
	n = fread(buf, 1, MAX_KEY_FILE, f);
	FreeFile(f);

	bio = BIO_new_mem_buf(buf, (int) n);
	if (bio == NULL)
		return false;
	while (!ok && count < MAX_KEYS &&
		   (key = PEM_read_bio_PUBKEY(bio, NULL, NULL, NULL)) != NULL)
	{
		count++;
		if (EVP_PKEY_id(key) == EVP_PKEY_ED25519)
		{
			EVP_MD_CTX *ctx = EVP_MD_CTX_new();

			if (ctx != NULL &&
				EVP_DigestVerifyInit(ctx, NULL, NULL, NULL, key) == 1 &&
				EVP_DigestVerify(ctx, sig, siglen, (const unsigned char *) input, inputlen) == 1)
				ok = true;
			EVP_MD_CTX_free(ctx);
		}
		EVP_PKEY_free(key);
	}
	BIO_free(bio);
	pfree(buf);
	if (count == 0)
		ereport(LOG, (errmsg("lattis_oauth : aucune clé publique dans « %s »", public_keys_file)));
	return ok;
}

static bool
validate_token(const ValidatorModuleState *state, const char *token,
			   const char *role, ValidatorModuleResult *result)
{
	const char *dot1;
	const char *dot2;
	char	   *header_json;
	char	   *payload_json;
	unsigned char *sig;
	int			siglen;
	Jsonb	   *header;
	Jsonb	   *payload;
	char	   *s;
	char	   *sub;
	int64		exp;
	int64		nbf;
	int64		now = (int64) time(NULL);
	size_t		toklen = strlen(token);

	result->authorized = false;
	result->authn_id = NULL;

	if (toklen > MAX_TOKEN)
		REJECT("trop long");
	dot1 = strchr(token, '.');
	dot2 = dot1 ? strchr(dot1 + 1, '.') : NULL;
	if (dot1 == NULL || dot2 == NULL || strchr(dot2 + 1, '.') != NULL)
		REJECT("pas un JWT");

	header_json = decode_segment(token, dot1 - token, NULL);
	payload_json = decode_segment(dot1 + 1, dot2 - dot1 - 1, NULL);
	sig = palloc(toklen);
	siglen = b64url_decode(dot2 + 1, strlen(dot2 + 1), sig);
	if (header_json == NULL || payload_json == NULL || siglen != 64)
		REJECT("encodage invalide");

	header = parse_json(header_json);
	payload = parse_json(payload_json);
	if (header == NULL || payload == NULL || !JB_ROOT_IS_OBJECT(payload))
		REJECT("JSON invalide");

	s = get_string(header, "alg");
	if (s == NULL || strcmp(s, "EdDSA") != 0)
		REJECT("algorithme non accepté");
	s = get_string(header, "typ");
	if (s == NULL || strcmp(s, "at+jwt") != 0)
		REJECT("pas un jeton d'accès");

	if (!signature_valid(token, dot2 - token, sig, siglen))
		REJECT("signature invalide");

	/* À partir d'ici, le jeton vient de lattisd : on peut nommer l'utilisateur. */
	sub = get_string(payload, "sub");
	if (sub == NULL || sub[0] == '\0')
		REJECT("sub manquant");
	result->authn_id = psprintf("lattis:%s", sub);

	s = get_string(payload, "iss");
	if (issuer == NULL || issuer[0] == '\0' || s == NULL || strcmp(s, issuer) != 0)
		REJECT("émetteur inattendu (%s)", s ? s : "aucun");
	if (!audience_matches(payload, audience))
		REJECT("audience inattendue");
	if (!get_int64(payload, "exp", &exp) || now > exp + leeway)
		REJECT("expiré");
	if (get_int64(payload, "nbf", &nbf) && now + leeway < nbf)
		REJECT("pas encore valide");
	s = get_string(payload, "scope");
	if (s == NULL || !scope_contains(s, required_scope))
		REJECT("scope « %s » absent", required_scope);
	s = get_string(payload, "pg_role");
	if (s == NULL || strcmp(s, role) != 0)
		REJECT("jeton émis pour le rôle « %s », connexion demandée en « %s »", s ? s : "", role);

	result->authorized = true;
	return true;
}
