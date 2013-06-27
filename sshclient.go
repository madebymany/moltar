package main

import (
	"bufio"
	"bytes"
	"code.google.com/p/go.crypto/ssh"
	"io"
	"log"
)

func sshDial(hostname string, username string, keyfile string) (conn *ssh.ClientConn, err error) {

	keyring := new(SSHKeyring)
	err = keyring.LoadPEM(keyfile)
	if err != nil {
		return
	}

	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.ClientAuth{
			ssh.ClientAuthKeyring(keyring),
		},
	}

	conn, err = ssh.Dial("tcp", hostname, config)
	return
}

func sshRunOutput(conn *ssh.ClientConn, cmd string) (output string, err error) {
	session, err := conn.NewSession()
	if err != nil {
		return
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b
	err = session.Run(cmd)
	if err != nil {
		return
	}
	return b.String(), nil
}

func sshRunOutLogger(conn *ssh.ClientConn, cmd string, logger *log.Logger) (term chan bool, loggerReturn chan error, err error) {
	session, err := conn.NewSession()
	if err != nil {
		return
	}

	/* We have to request a pty so that our command exits when the session
	 * closes. Ideally we'd send a TERM signal for the Session using
	 * session.Signal(ssh.SIGTERM), but OpenSSH doesn't support that yet:
	 * https://bugzilla.mindrot.org/show_bug.cgi?id=1424
	 */
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}
	err = session.RequestPty("xterm", 80, 40, modes)
	if err != nil {
		return
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return
	}
	stdout := bufio.NewReader(stdoutPipe)

	err = session.Start(cmd)
	if err != nil {
		return
	}

	term = make(chan bool, 1)
	loggerReturn = make(chan error, 1)

	go func() {
		var err error // shadow outer err
		shouldTerm := <-term
		if shouldTerm {
			// We can't use this, as OpenSSH doesn't support it. See above.
			/* err = session.Signal(ssh.SIGTERM)
			 * if err != nil {
			 *     logger.Println("remote terminantion error: " + err.Error())
			 * } */
			// We have to just close the session instead.
			err = session.Close()
			if err != nil {
				logger.Println("session close error: " + err.Error())
			}
		}
	}()

	go func() {
		defer session.Close()

		var line string
		var err error // shadow outer err

		for {
			line, err = stdout.ReadString('\n')
			if (err == io.EOF && line != "") || err == nil {
				logger.Print(line)
			}

			if err != nil {
				break
			}
		}

		err = session.Wait()
		close(term)

		if exitError, ok := err.(*ssh.ExitError); ok && exitError.Signal() == "HUP" {
			err = nil
		}

		loggerReturn <- err
	}()

	return
}
