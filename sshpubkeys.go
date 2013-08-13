package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"io/ioutil"
)

// SSHKeyring implements the ClientKeyring interface
type SSHKeyring struct {
	keys []*rsa.PrivateKey
}

func (k *SSHKeyring) Key(i int) (interface{}, error) {
	if i < 0 || i >= len(k.keys) {
		return nil, nil
	}
	return &k.keys[i].PublicKey, nil
}

func (k *SSHKeyring) Sign(i int, rand io.Reader, data []byte) (sig []byte, err error) {
	hashFunc := crypto.SHA1
	h := hashFunc.New()
	h.Write(data)
	var digest []byte
	digest = h.Sum(digest)
	return rsa.SignPKCS1v15(rand, k.keys[i], hashFunc, digest)
}

func (k *SSHKeyring) LoadPEM(file string) error {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(buf)
	if block == nil {
		return errors.New("ssh: no key found")
	}
	r, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	k.keys = append(k.keys, r)
	return nil
}
