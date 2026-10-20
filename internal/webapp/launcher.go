package webapp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Spec : ce qu'il faut pour lancer le moteur d'un utilisateur.
type Spec struct {
	UserID string
	// Secret : secret du moteur (LATTIS_ENGINE_SECRET), qu'il présente à
	// /engine/token pour obtenir les jetons de l'utilisateur.
	Secret string
	// Server : émetteur (seul serveur joignable par le moteur) ; TokenURL :
	// adresse de /engine/token vue du moteur ; Idle : inactivité avant arrêt.
	Server   string
	TokenURL string
	Idle     time.Duration
	// Demo : moteur de la démo publique (--public-demo) : ni serveur, ni
	// jeton, ni secret ; rien n'est gardé après son arrêt.
	Demo bool
}

// EngineArgs : options de lattis-engine pour s, écoute addr et répertoire dir.
func EngineArgs(s Spec, addr, dir string) []string {
	if s.Demo {
		return []string{"--addr", addr, "--dir", dir, "--base-path", DemoBase, "--public-demo", "--idle", s.Idle.String()}
	}
	return []string{"--addr", addr, "--dir", dir, "--base-path", "/app",
		"--server", s.Server, "--token-url", s.TokenURL, "--idle", s.Idle.String()}
}

// Instance : un moteur lancé.
type Instance interface {
	// Addr : hôte:port où lattisd joint le moteur (HTTP, base /app).
	Addr() string
	// Done se ferme quand le moteur s'est arrêté (inactivité, arrêt, panne).
	Done() <-chan struct{}
	// Stop demande un arrêt propre ; Kill arrête tout de suite.
	Stop()
	Kill()
}

// Launcher lance des moteurs : processus local, conteneur Docker (par le
// mini lanceur lattis-launcher) ou pod Kubernetes.
//
// Le Manager ne connaît que cette interface. Une réserve (pool) de moteurs
// démarrés d'avance sera un Launcher de plus, posé devant un autre : il
// gardera des instances prêtes et leur attribuera un utilisateur (Spec) à
// la demande, sans rien changer au Manager. Cela demande que lattis-engine
// accepte de recevoir l'utilisateur après son démarrage.
type Launcher interface {
	Start(ctx context.Context, s Spec) (Instance, error)
}

// Cleaner : lanceur qui sait retirer les moteurs laissés par une instance
// précédente de lattisd (conteneurs) ; appelé au démarrage.
type Cleaner interface {
	Cleanup(ctx context.Context) error
}

// waitReady attend que le moteur accepte les connexions sur addr.
func waitReady(ctx context.Context, addr string, done <-chan struct{}) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("le moteur s'est arrêté au démarrage")
		case <-ctx.Done():
			return fmt.Errorf("le moteur ne répond pas sur %s : %w", addr, ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// watchPeriod : fréquence de surveillance des moteurs distants.
var watchPeriod = 10 * time.Second

// watch ferme done quand alive répond faux (moteur parti), en le
// demandant toutes les period ; stop met fin à la surveillance.
func watch(period time.Duration, alive func(context.Context) (bool, error), done chan struct{}, stop <-chan struct{}) {
	t := time.NewTicker(period)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-stop:
			close(done)
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := alive(ctx)
		cancel()
		switch {
		case err != nil:
			// Panne passagère du lanceur ou de l'API : trois de suite, abandon.
			if misses++; misses < 3 {
				continue
			}
			close(done)
			return
		case !ok:
			close(done)
			return
		}
		misses = 0
	}
}

// onceFlag : vrai au premier set seulement.
type onceFlag struct {
	mu   sync.Mutex
	done bool
}

func (o *onceFlag) set() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return false
	}
	o.done = true
	return true
}
