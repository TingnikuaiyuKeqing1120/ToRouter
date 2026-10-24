package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func startupLock(dataDir, name string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dataDir, ".startup-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		locked, err := tryStartupLock(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if locked {
			var once sync.Once
			return func() { once.Do(func() { releaseStartupLock(file); file.Close() }) }, nil
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, fmt.Errorf("timed out waiting for startup lock %s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
