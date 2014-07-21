package main

import (
	"bufio"
	"code.google.com/p/gosshold/ssh"
	"io"
	"log"
	"net"
	"os"
	"strings"
)

func getSshAgent() (agent *ssh.AgentClient, err error) {
	conn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return
	}
	agent = ssh.NewAgentClient(conn)
	return
}

func sshDial(hostname string, username string, keyfile string) (conn *ssh.ClientConn, err error) {

	agent, err := getSshAgent()
	if err != nil {
		return nil, err
	}

	auths := []ssh.ClientAuth{}

	keyring := new(SSHKeyring)
	err = keyring.LoadPEM(keyfile)
	if err == nil {
		auths = append(auths, ssh.ClientAuthKeyring(keyring))
	}

	auths = append(auths, ssh.ClientAuthAgent(agent))
	conn, err = ssh.Dial("tcp", hostname,
		&ssh.ClientConfig{User: username, Auth: auths})
	return
}

func sshRunOutput(conn *ssh.ClientConn, cmd string) (output string, err error) {
	session, err := conn.NewSession()
	if err != nil {
		return
	}
	defer session.Close()

	b, err := session.Output(cmd)
	if err != nil {
		return
	}
	return string(b), nil
}

func sshRunOutLogger(conn *ssh.ClientConn, cmd string, logger *log.Logger, stdinChannel chan []byte) (term chan bool, loggerReturn chan error, err error) {
	session, err := conn.NewSession()
	if err != nil {
		return
	}

	if StdinIsTerminal() {
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

		terminal := os.Getenv("TERM")
		if terminal == "" {
			terminal = "xterm"
		}
		err = session.RequestPty(terminal, 80, 40, modes)
		if err != nil {
			return
		}
	} else {
		logger.Println("[WARNING] pty not requested because stdin is not a terminal")
	}

	var stdinPipe io.WriteCloser
	if stdinChannel != nil {
		stdinPipe, err = session.StdinPipe()
		if err != nil {
			return
		}
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return
	}
	stdout := channelFromReader(stdoutPipe)

	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return
	}
	stderr := channelFromReader(stderrPipe)

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

		for {
			select {
			case inBytes, ok := <-stdinChannel:
				if ok {
					stdinPipe.Write(inBytes)
				} else {
					stdinPipe.Close()
					stdinChannel = nil
				}
			case line, ok := <-stdout:
				if ok {
					logger.Println(line)
				} else {
					stdout = nil
				}
			case line, ok := <-stderr:
				if ok {
					logger.Println(line)
				} else {
					stderr = nil
				}
			}

			if stdinChannel == nil && stdout == nil && stderr == nil {
				break
			}
		}

		err := session.Wait()
		close(term)

		if exitError, ok := err.(*ssh.ExitError); ok && exitError.Signal() == "HUP" {
			err = nil
		}

		loggerReturn <- err
	}()

	return
}

func cleanOutputLine(line string) (out []string) {
	line = strings.TrimSpace(line)
	line = strings.Replace(line, "\r", "\n", -1)
	line = strings.Replace(line, "\n\n", "\n", -1)
	return strings.Split(line, "\n")
}

func channelFromReader(pipe io.Reader) (ch chan string) {
	ch = make(chan string)

	go func() {
		reader := bufio.NewReader(pipe)
		for {
			in, err := reader.ReadString('\n')
			if (err == io.EOF && in != "") || err == nil {
				lines := cleanOutputLine(in)
				for _, line := range lines {
					ch <- line
				}
			}

			if err != nil {
				close(ch)
				return
			}
		}
	}()

	return
}
