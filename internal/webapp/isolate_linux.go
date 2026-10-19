//go:build linux

package webapp

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// isolate : le moteur meurt avec lattisd et, avec un UID (>= 0), tourne
// sous cet utilisateur et ce groupe, sans groupes supplémentaires.
// lattisd a alors besoin de CAP_SETUID, CAP_SETGID, CAP_CHOWN, CAP_FOWNER, CAP_KILL
// (et CAP_DAC_OVERRIDE pour ses volumes, à l'utilisateur 65532).
func isolate(cmd *exec.Cmd, uid int) {
	attr := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if uid >= 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), NoSetGroups: true}
	}
	cmd.SysProcAttr = attr
}

// ownTree donne le répertoire d'un moteur à son UID (s'il ne l'a pas déjà).
func ownTree(dir string, uid int) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && int(s.Uid) == uid && int(s.Gid) == uid {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, uid)
	})
}
