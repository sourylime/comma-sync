package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
)

func sshKeyPath() string {
	if k := os.Getenv("SSH_KEY"); k != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "id_ed25519")
}

// dial opens an SSH connection to the comma using the local key. Host keys are
// ignored on purpose: this is a LAN device whose key changes on every reflash
// (same rationale as StrictHostKeyChecking=no in comma-sync.sh).
func dial(host string, port int, timeout time.Duration) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(sshKeyPath())
	if err != nil {
		return nil, fmt.Errorf("reading SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            remoteUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}
	return ssh.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)), cfg)
}

// runCmd runs a command over an existing SSH client and returns combined output.
func runCmd(c *ssh.Client, cmd string) (string, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}
