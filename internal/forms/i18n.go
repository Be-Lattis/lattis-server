package forms

// tr : textes de la page dans une langue.
type tr map[string]string

var texts = map[string]tr{
	"fr": {
		"required_mark": "* obligatoire", "required": "Ce champ est obligatoire.", "must_check": "Cette case doit être cochée.",
		"submit": "Envoyer", "not_in_list": "Choisissez une valeur de la liste.", "bad_value": "Valeur incorrecte.",
		"number": "Nombre attendu.", "integer": "Nombre entier attendu.", "min": "Au moins %s.", "max": "Au plus %s.",
		"date": "Date attendue (jj/mm/aaaa).", "datetime": "Date et heure attendues.", "too_long": "%d caractères au plus.",
		"email": "Adresse e-mail incorrecte.", "phone": "Numéro de téléphone incorrect.", "url": "Adresse web attendue (https://…).",
		"position": "Position incorrecte.", "my_position": "Ma position", "latitude": "Latitude", "longitude": "Longitude",
		"locating": "Recherche de la position…", "located": "Position trouvée (précision : %s m).", "no_position": "Position indisponible.",
		"yes": "Oui", "no": "Non", "choose": "Choisir…", "errors": "Corrigez les champs signalés.",
		"thanks": "Merci, votre réponse est enregistrée.", "another": "Envoyer une autre réponse",
		"not_found": "Ce formulaire n'existe pas ou n'est pas accessible.", "not_found_title": "Formulaire introuvable",
		"closed": "Ce formulaire est fermé.", "not_open": "Ce formulaire n'est pas encore ouvert.", "full": "Ce formulaire a reçu toutes les réponses attendues.",
		"already": "Vous avez déjà répondu à ce formulaire.", "taken": "Une valeur saisie existe déjà dans le tableau : modifiez-la.",
		"too_fast": "Envoi trop rapide : patientez quelques secondes puis renvoyez.", "expired": "La page a expiré : vérifiez vos réponses et renvoyez.",
		"rate": "Trop d'envois : réessayez dans une minute.", "preview": "Aperçu : les réponses ne sont pas enregistrées.",
		"preview_done": "Aperçu : la réponse est valide, mais elle n'a pas été enregistrée.", "offline": "Pas de réseau : votre réponse est gardée sur ce téléphone.",
		"retry": "Réessayer", "error": "Le serveur n'a pas pu enregistrer la réponse : réessayez.", "powered": "Formulaire Lattis",
		"locked": "Valeur fixée par le lien.",
	},
	"en": {
		"required_mark": "* required", "required": "This field is required.", "must_check": "This box must be checked.",
		"submit": "Submit", "not_in_list": "Choose a value from the list.", "bad_value": "Incorrect value.",
		"number": "A number is expected.", "integer": "A whole number is expected.", "min": "At least %s.", "max": "At most %s.",
		"date": "A date is expected.", "datetime": "A date and time are expected.", "too_long": "%d characters at most.",
		"email": "Incorrect email address.", "phone": "Incorrect phone number.", "url": "A web address is expected (https://…).",
		"position": "Incorrect position.", "my_position": "My location", "latitude": "Latitude", "longitude": "Longitude",
		"locating": "Locating…", "located": "Location found (accuracy: %s m).", "no_position": "Location unavailable.",
		"yes": "Yes", "no": "No", "choose": "Choose…", "errors": "Please correct the highlighted fields.",
		"thanks": "Thank you, your response has been recorded.", "another": "Submit another response",
		"not_found": "This form does not exist or is not available.", "not_found_title": "Form not found",
		"closed": "This form is closed.", "not_open": "This form is not open yet.", "full": "This form has received all expected responses.",
		"already": "You have already responded to this form.", "taken": "A value you entered already exists in the table: please change it.",
		"too_fast": "Sent too quickly: wait a few seconds and submit again.", "expired": "The page has expired: check your answers and submit again.",
		"rate": "Too many submissions: try again in a minute.", "preview": "Preview: responses are not recorded.",
		"preview_done": "Preview: the response is valid but was not recorded.", "offline": "No network: your response is kept on this device.",
		"retry": "Retry", "error": "The server could not record the response: please try again.", "powered": "Lattis form",
		"locked": "Value set by the link.",
	},
}

func lang(l string) tr {
	if t, ok := texts[l]; ok {
		return t
	}
	return texts["fr"]
}
