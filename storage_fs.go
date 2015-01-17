/* Copyright (c) 2014-2015, Daniel Martí <mvdan@mvdan.cc> */
/* See LICENSE for licensing information */

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type FileStore struct {
	sync.RWMutex
	cache map[ID]fileCache

	dir   string
	stats Stats
}

type fileCache struct {
	reading sync.WaitGroup
	header  Header
	path    string
}

type FileContent struct {
	file    *os.File
	reading *sync.WaitGroup
}

func (c FileContent) Read(p []byte) (n int, err error) {
	return c.file.Read(p)
}

func (c FileContent) ReadAt(p []byte, off int64) (n int, err error) {
	return c.file.ReadAt(p, off)
}

func (c FileContent) Seek(offset int64, whence int) (int64, error) {
	return c.file.Seek(offset, whence)
}

func (c FileContent) Close() error {
	err := c.file.Close()
	c.reading.Done()
	return err
}

func newFileStore(dir string) (s *FileStore, err error) {
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chdir(dir); err != nil {
		return nil, err
	}
	s = new(FileStore)
	s.dir = dir
	s.cache = make(map[ID]fileCache)
	for i := 0; i < 256; i++ {
		if err = s.setupSubdir(byte(i)); err != nil {
			return nil, err
		}
	}
	return
}

func (s *FileStore) Get(id ID) (Content, *Header, error) {
	s.RLock()
	defer s.RUnlock()
	cached, e := s.cache[id]
	if !e {
		return nil, nil, ErrPasteNotFound
	}
	f, err := os.Open(cached.path)
	if err != nil {
		return nil, nil, err
	}
	cached.reading.Add(1)
	return FileContent{f, &cached.reading}, &cached.header, nil
}

func writeNewFile(filename string, data []byte) error {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

func (s *FileStore) Put(content []byte) (id ID, err error) {
	s.Lock()
	defer s.Unlock()
	size := int64(len(content))
	if !s.stats.hasSpaceFor(size) {
		return id, ErrReachedMax
	}
	if id, err = s.randomID(); err != nil {
		return
	}
	hexID := id.String()
	pastePath := path.Join(hexID[:2], hexID[2:])
	if err = writeNewFile(pastePath, content); err != nil {
		return
	}
	s.stats.makeSpaceFor(size)
	s.cache[id] = fileCache{
		header: genHeader(id, time.Now(), size),
		path:   pastePath,
	}
	return id, nil
}

func (s *FileStore) Delete(id ID) error {
	s.Lock()
	defer s.Unlock()
	cached, e := s.cache[id]
	if !e {
		return ErrPasteNotFound
	}
	delete(s.cache, id)
	s.stats.freeSpace(cached.header.Size)
	cached.reading.Wait()
	if err := os.Remove(cached.path); err != nil {
		return err
	}
	return nil
}

func (s *FileStore) Recover(pastePath string, fileInfo os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if fileInfo.IsDir() {
		return nil
	}
	parts := strings.Split(pastePath, string(filepath.Separator))
	if len(parts) != 2 {
		return errors.New("invalid number of directories at " + pastePath)
	}
	hexID := parts[0] + parts[1]
	id, err := IDFromString(hexID)
	if err != nil {
		return err
	}
	modTime := fileInfo.ModTime()
	deathTime := modTime.Add(lifeTime)
	if lifeTime > 0 {
		if deathTime.Before(startTime) {
			return os.Remove(pastePath)
		}
	}
	size := fileInfo.Size()
	s.Lock()
	defer s.Unlock()
	if !s.stats.hasSpaceFor(size) {
		return ErrReachedMax
	}
	s.stats.makeSpaceFor(size)
	lifeLeft := deathTime.Sub(startTime)
	cached := fileCache{
		header: genHeader(id, modTime, size),
		path:   pastePath,
	}
	s.cache[id] = cached
	SetupPasteDeletion(s, id, lifeLeft)
	return nil
}

func (s *FileStore) randomID() (id ID, err error) {
	for try := 0; try < randTries; try++ {
		if _, err := rand.Read(id[:]); err != nil {
			continue
		}
		if _, e := s.cache[id]; !e {
			return id, nil
		}
	}
	return id, ErrNoUnusedIDFound
}

func (s *FileStore) setupSubdir(h byte) error {
	dir := hex.EncodeToString([]byte{h})
	if stat, err := os.Stat(dir); err == nil {
		if !stat.IsDir() {
			return fmt.Errorf("%s/%s exists but is not a directory", s.dir, dir)
		}
		if err := filepath.Walk(dir, s.Recover); err != nil {
			return fmt.Errorf("cannot recover data directory %s/%s: %s", s.dir, dir, err)
		}
	} else if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("cannot create data directory %s/%s: %s", s.dir, dir, err)
	}
	return nil
}

func (s *FileStore) Report() string {
	s.Lock()
	defer s.Unlock()
	return s.stats.Report()
}
