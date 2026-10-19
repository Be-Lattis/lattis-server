package webapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

// KubeLauncher lance chaque moteur dans un pod, par l'API Kubernetes (compte
// de service du pod de lattisd : droits pods et persistentvolumeclaims de
// son espace de noms, voir le chart Helm). Les pods des moteurs
// appartiennent au pod de lattisd : ils disparaissent avec lui.
type KubeLauncher struct {
	Cfg config.K8sEngines
	// API : adresse de l'API (https://kubernetes.default.svc par défaut) ;
	// TokenFile, CAFile : identité du compte de service.
	API, TokenFile, CAFile string
	HTTP                   *http.Client
	// Port : port du moteur dans son pod (8102 ; changé par les tests).
	Port string
}

const (
	saDir      = "/var/run/secrets/kubernetes.io/serviceaccount"
	engineHTTP = "8102"
	labelApp   = "app.kubernetes.io/name"
	labelUser  = "lattis.dev/user"
	labelOwner = "lattis.dev/owner"
)

// NewKubeLauncher : lanceur dans le cluster (compte de service monté).
func NewKubeLauncher(cfg config.K8sEngines) (*KubeLauncher, error) {
	k := &KubeLauncher{Cfg: cfg, TokenFile: saDir + "/token", CAFile: saDir + "/ca.crt"}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, errors.New("LATTIS_WEB_LAUNCHER=kubernetes : lattisd ne tourne pas dans un cluster (KUBERNETES_SERVICE_HOST absent)")
	}
	k.API = "https://" + net.JoinHostPort(host, port)
	ca, err := os.ReadFile(k.CAFile)
	if err != nil {
		return nil, fmt.Errorf("autorité du cluster : %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	k.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	if k.Cfg.Namespace == "" {
		ns, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("espace de noms : %w", err)
		}
		k.Cfg.Namespace = strings.TrimSpace(string(ns))
	}
	if k.Cfg.OwnerPod == "" {
		k.Cfg.OwnerPod, _ = os.Hostname()
	}
	return k, nil
}

func (k *KubeLauncher) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.API+path, rd)
	if err != nil {
		return 0, err
	}
	// Jeton relu à chaque appel : Kubernetes le fait tourner.
	if k.TokenFile != "" {
		tok, err := os.ReadFile(k.TokenFile)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("API Kubernetes : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&st)
		return resp.StatusCode, fmt.Errorf("API Kubernetes : %s %s : %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, st.Message)
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

func (k *KubeLauncher) ns() string { return "/api/v1/namespaces/" + url.PathEscape(k.Cfg.Namespace) }

func (k *KubeLauncher) selector(user string) string {
	s := labelApp + "=lattis-engine," + labelOwner + "=" + k.Cfg.OwnerPod
	if user != "" {
		s += "," + labelUser + "=" + user
	}
	return url.QueryEscape(s)
}

func (k *KubeLauncher) deleteAll(ctx context.Context, user string) error {
	_, err := k.call(ctx, http.MethodDelete, k.ns()+"/pods?labelSelector="+k.selector(user), nil, nil)
	return err
}

// Cleanup retire les pods laissés par ce pod de lattisd avant un redémarrage
// du processus (un pod de lattisd supprimé emporte les siens tout seul).
func (k *KubeLauncher) Cleanup(ctx context.Context) error { return k.deleteAll(ctx, "") }

// PodSpec : pod du moteur d'un utilisateur (exporté pour les tests).
func (k *KubeLauncher) PodSpec(s Spec, name string) map[string]any {
	c := k.Cfg
	meta := map[string]any{"name": name, "labels": map[string]string{
		labelApp: "lattis-engine", "app.kubernetes.io/component": "engine", labelUser: s.UserID, labelOwner: c.OwnerPod}}
	if c.OwnerPod != "" && c.OwnerUID != "" {
		meta["ownerReferences"] = []map[string]any{{"apiVersion": "v1", "kind": "Pod", "name": c.OwnerPod, "uid": c.OwnerUID}}
	}
	data := map[string]any{"name": "data", "emptyDir": map[string]any{}}
	if c.CacheSize != "" {
		data = map[string]any{"name": "data", "persistentVolumeClaim": map[string]string{"claimName": "lattis-web-" + s.UserID}}
	}
	resources := map[string]any{"requests": map[string]string{"cpu": c.CPU, "memory": c.Memory},
		"limits": map[string]string{"memory": c.Memory}}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 10,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers": []map[string]any{{
			"name": "engine", "image": c.Image,
			"args": []string{"--addr", "0.0.0.0:" + engineHTTP, "--dir", "/data", "--base-path", "/app",
				"--server", s.Server, "--token-url", s.TokenURL, "--idle", s.Idle.String()},
			// Entrée standard ouverte : le moteur s'arrête quand elle se ferme.
			"stdin":     true,
			"env":       []map[string]string{{"name": "LATTIS_ENGINE_SECRET", "value": s.Secret}, {"name": "HOME", "value": "/data"}},
			"ports":     []map[string]any{{"name": "http", "containerPort": 8102}},
			"resources": resources,
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}}},
			"volumeMounts": []map[string]string{{"name": "data", "mountPath": "/data"}, {"name": "tmp", "mountPath": "/tmp"}},
		}},
		"volumes": []map[string]any{data, {"name": "tmp", "emptyDir": map[string]string{"sizeLimit": "64Mi"}}},
	}
	if c.PullSecret != "" {
		spec["imagePullSecrets"] = []map[string]string{{"name": c.PullSecret}}
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec}
}

type podStatus struct {
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

func (k *KubeLauncher) Start(ctx context.Context, s Spec) (Instance, error) {
	if err := k.deleteAll(ctx, s.UserID); err != nil {
		return nil, err
	}
	if k.Cfg.CacheSize != "" {
		pvc := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": "lattis-web-" + s.UserID, "labels": map[string]string{labelApp: "lattis-engine", labelUser: s.UserID}},
			"spec": map[string]any{"accessModes": []string{"ReadWriteOnce"},
				"resources": map[string]any{"requests": map[string]string{"storage": k.Cfg.CacheSize}}}}
		if k.Cfg.StorageClass != "" {
			pvc["spec"].(map[string]any)["storageClassName"] = k.Cfg.StorageClass
		}
		if code, err := k.call(ctx, http.MethodPost, k.ns()+"/persistentvolumeclaims", pvc, nil); err != nil && code != http.StatusConflict {
			return nil, err
		}
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	name := "lattis-engine-" + hex.EncodeToString(b)
	if _, err := k.call(ctx, http.MethodPost, k.ns()+"/pods", k.PodSpec(s, name), nil); err != nil {
		return nil, err
	}
	pod := k.ns() + "/pods/" + name
	inst := &remoteInstance{done: make(chan struct{}), stop: make(chan struct{}),
		halt: func(kill bool) {
			grace := "10"
			if kill {
				grace = "0"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = k.call(ctx, http.MethodDelete, pod+"?gracePeriodSeconds="+grace, nil, nil)
		}}
	// Attente de l'adresse du pod (image tirée, volume monté).
	for {
		var st podStatus
		if _, err := k.call(ctx, http.MethodGet, pod, nil, &st); err != nil {
			inst.Kill()
			return nil, err
		}
		if st.Status.Phase == "Failed" || st.Status.Phase == "Succeeded" {
			inst.Kill()
			return nil, errors.New("le pod du moteur s'est arrêté au démarrage")
		}
		if st.Status.Phase == "Running" && st.Status.PodIP != "" {
			port := k.Port
			if port == "" {
				port = engineHTTP
			}
			inst.addr = net.JoinHostPort(st.Status.PodIP, port)
			break
		}
		select {
		case <-ctx.Done():
			inst.Kill()
			return nil, fmt.Errorf("le pod du moteur n'a pas démarré à temps : %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if err := waitReady(ctx, inst.addr, inst.done); err != nil {
		inst.Kill()
		return nil, err
	}
	go watch(watchPeriod, func(ctx context.Context) (bool, error) {
		var st podStatus
		code, err := k.call(ctx, http.MethodGet, pod, nil, &st)
		if code == http.StatusNotFound {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return st.Status.Phase == "Running" || st.Status.Phase == "Pending", nil
	}, inst.done, inst.stop)
	return inst, nil
}
