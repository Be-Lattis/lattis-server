package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// PoolPrefix : identifiant des moteurs de la réserve (pas encore attribués) ;
// leur répertoire de travail est éphémère, effacé avec eux.
const PoolPrefix = "pool-"

// Pool garde Spare moteurs démarrés d'avance, prêts à recevoir un
// utilisateur, devant un autre lanceur. Un moteur attribué sert une seule
// personne puis disparaît (inactivité, arrêt) : il n'est jamais rendu à la
// réserve, qui en démarre un neuf. Moteurs attribués et moteurs prêts
// restent ensemble sous Max (LATTIS_WEB_MAX_ENGINES).
//
// Un moteur démarre sans utilisateur : il ne demande ses jetons qu'à la
// première connexion à PostgreSQL. Le Manager rattache son secret à
// l'utilisateur au moment de l'attribution (Instance.Secret).
type Pool struct {
	Inner Launcher
	Spare int
	Max   int
	// Template : serveur, adresse des jetons et inactivité des moteurs.
	Template Spec

	mu       sync.Mutex
	ready    []*poolInstance
	starting int
	assigned int
	stopped  bool
	refillCh chan struct{}
	once     sync.Once
	// memory : mémoire par moteur des prochains moteurs prêts (dernière
	// demandée) ; un moteur prêt d'une autre mémoire est remplacé.
	memory int64
}

// poolInstance : moteur de la réserve, avec son secret.
type poolInstance struct {
	Instance
	secret string
	memory int64
}

// Secret : secret que le moteur présente à /engine/token.
func (p *poolInstance) Secret() string { return p.secret }

func (p *Pool) init() {
	p.once.Do(func() {
		p.refillCh = make(chan struct{}, 1)
		go p.refillLoop()
	})
}

// Warm démarre la réserve (au lancement de lattisd).
func (p *Pool) Warm() {
	p.init()
	p.kick()
}

func (p *Pool) kick() {
	select {
	case p.refillCh <- struct{}{}:
	default:
	}
}

// Ready : moteurs prêts (mesures, tests).
func (p *Pool) Ready() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready)
}

// Cleanup délègue au lanceur sous-jacent.
func (p *Pool) Cleanup(ctx context.Context) error {
	if c, ok := p.Inner.(Cleaner); ok {
		return c.Cleanup(ctx)
	}
	return nil
}

// Start attribue un moteur prêt, ou en démarre un s'il n'y en a pas. Le
// secret de s est remplacé par celui du moteur (Instance.Secret).
func (p *Pool) Start(ctx context.Context, s Spec) (Instance, error) {
	p.init()
	p.mu.Lock()
	p.memory = s.Memory
	var inst *poolInstance
	for len(p.ready) > 0 && inst == nil {
		inst = p.ready[0]
		p.ready = p.ready[1:]
		select {
		case <-inst.Done(): // parti entre-temps
			inst = nil
		default:
			if inst.memory != s.Memory {
				// Réglage de mémoire changé depuis son démarrage.
				go inst.Stop()
				inst = nil
			}
		}
	}
	if inst != nil {
		p.assigned++
	} else if p.assigned+len(p.ready)+p.starting >= p.Max {
		// Plus de place : aucun moteur prêt, et pas de quoi en démarrer un.
		p.mu.Unlock()
		return nil, ErrBusy
	} else {
		p.starting++
	}
	p.mu.Unlock()
	if inst == nil {
		var err error
		inst, err = p.launch(ctx)
		p.mu.Lock()
		p.starting--
		if err == nil {
			p.assigned++
		}
		p.mu.Unlock()
		if err != nil {
			p.kick()
			return nil, err
		}
	}
	go func() {
		<-inst.Done()
		p.mu.Lock()
		p.assigned--
		p.mu.Unlock()
		p.kick()
	}()
	slog.Info("moteur web attribué", "user", s.UserID, "ready", p.Ready())
	p.kick()
	return inst, nil
}

func (p *Pool) launch(ctx context.Context) (*poolInstance, error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	spec := p.Template
	spec.UserID = PoolPrefix + hex.EncodeToString(b)
	spec.Secret = randomSecret()
	p.mu.Lock()
	spec.Memory = p.memory
	p.mu.Unlock()
	inst, err := p.Inner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &poolInstance{Instance: inst, secret: spec.Secret, memory: spec.Memory}, nil
}

// refillLoop ramène la réserve à Spare moteurs prêts, sans dépasser Max.
func (p *Pool) refillLoop() {
	for range p.refillCh {
		for {
			p.mu.Lock()
			need := !p.stopped && len(p.ready)+p.starting < p.Spare && p.assigned+len(p.ready)+p.starting < p.Max
			if need {
				p.starting++
			}
			p.mu.Unlock()
			if !need {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
			inst, err := p.launch(ctx)
			cancel()
			p.mu.Lock()
			p.starting--
			if err == nil && !p.stopped {
				p.ready = append(p.ready, inst)
			}
			stopped := p.stopped
			p.mu.Unlock()
			if err != nil {
				slog.Warn("réserve de moteurs web", "err", err)
				time.Sleep(5 * time.Second) // lanceur en panne : pas de boucle serrée
				break
			}
			if stopped {
				inst.Kill()
				break
			}
			// Un moteur prêt qui s'arrête (inactivité, panne) est remplacé.
			go func(i *poolInstance) {
				<-i.Done()
				p.mu.Lock()
				for n, r := range p.ready {
					if r == i {
						p.ready = append(p.ready[:n], p.ready[n+1:]...)
						break
					}
				}
				p.mu.Unlock()
				p.kick()
			}(inst)
		}
	}
}

// StopAll arrête les moteurs prêts (arrêt de lattisd).
func (p *Pool) StopAll() {
	p.mu.Lock()
	p.stopped = true
	ready := p.ready
	p.ready = nil
	p.mu.Unlock()
	for _, i := range ready {
		i.Stop()
	}
}

// isPool : identifiant d'un moteur de la réserve.
func isPool(user string) bool { return strings.HasPrefix(user, PoolPrefix) }
