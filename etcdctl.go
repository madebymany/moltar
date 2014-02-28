package main

import (
	"code.google.com/p/go.crypto/ssh"
	"encoding/json"
)

type etcdctlResponse struct {
	Node struct {
		Key           string
		Value         string
		ModifiedIndex uint64
		CreatedIndex  uint64
	}
}

func etcdctl(conn *ssh.ClientConn, cmd string) (resp etcdctlResponse, err error) {
	respStr, err := sshRunOutput(conn, "etcdctl -o json "+cmd)
	if err != nil {
		return
	}
	err = json.Unmarshal([]byte(respStr), &resp)
	return
}

func errIsExpectedEtcdError(err error) bool {
	waitmsg, ok := err.(*ssh.ExitError)
	return ok && waitmsg.ExitStatus() == 4
}
