package main

import (
	"github.com/kless/term"
	"os"
	"sync"
	"syscall"
)

var (
	stdinReading  bool
	stdinChannels [](chan []byte)
	stdinWait     sync.WaitGroup
)

func StdinIsTerminal() bool {
	return term.IsTerminal(syscall.Stdin)
}

func makeStdinChannel() (chanOut chan []byte) {
	if stdinReading {
		panic("already reading stdin")
	}
	chanOut = make(chan []byte)
	stdinChannels = append(stdinChannels, chanOut)
	return
}

func closeAllStdinChannels() {
	for _, ch := range stdinChannels {
		close(ch)
	}
}

func StartStdinRead() {
	if !StdinIsTerminal() {
		stdinWait.Done()
	}
}

func WaitForStdinStart(n int) {
	if StdinIsTerminal() {
		closeAllStdinChannels()
		return
	}

	stdinWait.Add(n)
	// FIXME: There's probably a deadlock here if some hosts error
	stdinWait.Wait()

	stdinReading = true

	bs := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(bs)
		if n > 0 {
			for _, ch := range stdinChannels {
				ch <- bs
			}
		} else if n == 0 || err != nil {
			closeAllStdinChannels()
			return
		}
	}
}
