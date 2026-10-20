package webapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProcessLauncher lance chaque moteur comme un processus de lattisd. Avec
// une plage d'UID (UIDs, ou par défaut quand lattisd tourne en root sous
// Linux), chaque moteur tourne sous son propre utilisateur système : il
// n'atteint ni la clé de signature, ni les secrets, ni les autres caches.
type ProcessLauncher struct {
	Bin      string
	CacheDir string
	// UIDs : première valeur et nombre ; {0, 0} : même utilisateur que lattisd.
	UIDs [2]int

	mu   sync.Mutex
	used map[int]string // UID → utilisateur
	pids map[int]bool   // moteurs en cours (mesure de la mémoire)
}

// DefaultUIDs : plage prise quand lattisd tourne en root sans
// LATTIS_WEB_ENGINE_UIDS (au moins max moteurs).
func DefaultUIDs(engines int) [2]int { return [2]int{200000, max(engines, 1000)} }

type procInstance struct {
	cmd   *exec.Cmd
	stdin io.Closer
	addr  string
	done  chan struct{}
}

func (p *procInstance) Addr() string          { return p.addr }
func (p *procInstance) Done() <-chan struct{} { return p.done }
func (p *procInstance) Stop()                 { _ = p.stdin.Close() }
func (p *procInstance) Kill()                 { _ = p.cmd.Process.Kill() }

func (l *ProcessLauncher) Start(ctx context.Context, s Spec) (Instance, error) {
	uid, err := l.reserve(s.UserID)
	if err != nil {
		return nil, err
	}
	inst, err := l.start(ctx, s, uid)
	if err != nil {
		l.release(uid, s.UserID)
		return nil, err
	}
	go func() {
		<-inst.done
		l.release(uid, s.UserID)
		if isPool(s.UserID) {
			// Répertoire d'un moteur de la réserve : éphémère.
			_ = os.RemoveAll(filepath.Join(l.CacheDir, s.UserID))
		}
	}()
	return inst, nil
}

func (l *ProcessLauncher) reserve(user string) (int, error) {
	if l.UIDs[1] == 0 {
		return -1, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used == nil {
		l.used = map[int]string{}
	}
	uid, ok := pickUID(l.UIDs[0], l.UIDs[1], l.used, user)
	if !ok {
		return 0, ErrBusy
	}
	l.used[uid] = user
	return uid, nil
}

func (l *ProcessLauncher) release(uid int, user string) {
	if uid < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used[uid] == user {
		delete(l.used, uid)
	}
}

func (l *ProcessLauncher) start(ctx context.Context, s Spec, uid int) (*procInstance, error) {
	dir := filepath.Join(l.CacheDir, s.UserID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if uid >= 0 {
		// Le parent reste traversable, pas lisible : chaque moteur
		// n'atteint que son propre répertoire.
		if err := os.Chmod(l.CacheDir, 0o711); err != nil {
			return nil, err
		}
		if err := ownTree(dir, uid); err != nil {
			return nil, fmt.Errorf("cache du moteur : %w", err)
		}
	}
	cmd := exec.Command(l.Bin,
		"--addr", "127.0.0.1:0", "--dir", dir, "--base-path", "/app",
		"--server", s.Server, "--token-url", s.TokenURL, "--idle", s.Idle.String())
	// Environnement minimal : rien des secrets de lattisd.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, ".cache"), "XDG_DATA_HOME=" + filepath.Join(dir, ".local", "share"),
		"LATTIS_ENGINE_SECRET=" + s.Secret}
	cmd.Dir = dir
	isolate(cmd, uid)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &logWriter{user: s.UserID}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lancement de %s : %w", l.Bin, err)
	}
	p := &procInstance{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	l.track(cmd.Process.Pid, true)
	go func() {
		_ = cmd.Wait()
		l.track(cmd.Process.Pid, false)
		close(p.done)
	}()

	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		found := false
		for sc.Scan() {
			line := sc.Text()
			if a, ok := strings.CutPrefix(line, "LISTENING "); ok && !found {
				found = true
				addr <- strings.TrimSpace(a)
				continue
			}
			slog.Info("moteur web", "user", s.UserID, "out", line)
		}
	}()
	select {
	case a := <-addr:
		host, _, err := net.SplitHostPort(a)
		if err != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
			p.Kill()
			return nil, fmt.Errorf("adresse du moteur inattendue : %q", a)
		}
		p.addr = a
		return p, nil
	case <-p.done:
		return nil, errors.New("le moteur s'est arrêté au démarrage")
	case <-ctx.Done():
		p.Kill()
		return nil, errors.New("le moteur n'a pas annoncé son adresse à temps")
	case <-time.After(startTimeout):
		p.Kill()
		return nil, errors.New("le moteur n'a pas annoncé son adresse à temps")
	}
}

// pickUID : UID de la plage pour un utilisateur, le même d'une fois sur
// l'autre tant qu'il est libre (son cache n'a alors pas à changer de
// propriétaire), sinon le suivant libre.
func pickUID(first, count int, used map[int]string, user string) (int, bool) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(user))
	start := int(h.Sum32() % uint32(count))
	for i := 0; i < count; i++ {
		uid := first + (start+i)%count
		if _, taken := used[uid]; !taken {
			return uid, true
		}
	}
	return 0, false
}

// logWriter recopie la sortie d'erreur du moteur dans le journal.
type logWriter struct{ user string }

func (l *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			slog.Warn("moteur web", "user", l.user, "err", line)
		}
	}
	return len(p), nil
}
