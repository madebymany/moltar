package main

import (
	"code.google.com/p/gosshold/ssh"
	"io"
	"io/ioutil"
)

// SSHKeyring implements the ClientKeyring interface
type SSHKeyring struct {
	keys []ssh.Signer
}

func (k *SSHKeyring) Key(i int) (ssh.PublicKey, error) {
	if i < 0 || i >= len(k.keys) {
		return nil, nil
	}
	return k.keys[i].PublicKey(), nil
}

func (k *SSHKeyring) Sign(i int, rand io.Reader, data []byte) (sig []byte, err error) {
	return k.keys[i].Sign(rand, data)
}

func (k *SSHKeyring) LoadPEM(file string) error {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}

	r, err := ssh.ParsePrivateKey(buf)
	if err != nil {
		return err
	}

	k.keys = append(k.keys, r)
	return nil
}
