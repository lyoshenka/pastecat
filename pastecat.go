/* Copyright (c) 2014, Daniel Martí <mvdan@mvdan.cc> */
/* See LICENSE for licensing information */

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	indexTmpl = "index.html"
	formTmpl  = "form.html"
	idSize    = 8
	rawIdSize = idSize / 2
	randTries = 10

	// GET error messages
	invalidId     = "Invalid paste id."
	pasteNotFound = "Paste doesn't exist."
	unknownError  = "Something went terribly wrong."
	// POST error messages
	missingForm = "Paste could not be found inside the posted form."
)

var (
	siteUrl, listen, dataDir, maxSizeStr string
	lifeTime                             time.Duration
	maxSize                              ByteSize
	indexTemplate, formTemplate          *template.Template

	regexByteSize = regexp.MustCompile(`^([\d\.]+)\s*([KM]?B|[BKM])$`)
)

type Id [rawIdSize]byte

type PasteInfo struct {
	Path, Etag, ContentType string
	ModTime                 time.Time
}

var pastes struct {
	sync.RWMutex
	m map[Id]PasteInfo
}

func init() {
	flag.StringVar(&siteUrl, "u", "http://localhost:8080", "URL of the site")
	flag.StringVar(&listen, "l", "localhost:8080", "Host and port to listen to")
	flag.StringVar(&dataDir, "d", "data", "Directory to store all the pastes in")
	flag.DurationVar(&lifeTime, "t", 12*time.Hour, "Lifetime of the pastes (units: s,m,h)")
	flag.StringVar(&maxSizeStr, "s", "1M", "Maximum size of POSTs in bytes (units: B,K,M)")
	pastes.m = make(map[Id]PasteInfo)
}

func IdFromString(hexId string) (Id, error) {
	var id Id
	if len(hexId) != idSize {
		return id, errors.New("Invalid id")
	}
	b, err := hex.DecodeString(hexId)
	if err != nil || len(b) != rawIdSize {
		return id, errors.New("Invalid id")
	}
	copy(id[:], b)
	return id, nil
}

func IdFromPath(idPath string) (Id, error) {
	var id Id
	parts := strings.Split(idPath, string(filepath.Separator))
	if len(parts) != 2 {
		return id, errors.New("Found invalid number of directories at " + idPath)
	}
	return IdFromString(parts[0] + parts[1])
}

func RandomId() (Id, error) {
	var id Id
	for try := 0; try < randTries; try++ {
		if _, err := rand.Read(id[:]); err != nil {
			return id, err
		}
		if _, e := pastes.m[id]; !e {
			return id, nil
		}
	}
	return id, fmt.Errorf("Gave up trying to find an unused random id after %d tries", randTries)
}

func (id Id) String() string {
	return hex.EncodeToString(id[:])
}

func (id Id) Path() string {
	hexId := id.String()
	return path.Join(hexId[0:2], hexId[2:])
}

func (id Id) GenPasteInfo(modTime time.Time, head []byte) (pasteInfo PasteInfo) {
	pasteInfo.ModTime = modTime
	pasteInfo.Etag = fmt.Sprintf("%d-%s", pasteInfo.ModTime.Unix(), id)
	pasteInfo.ContentType = http.DetectContentType(head)
	if pasteInfo.ContentType == "application/octet-stream" {
		pasteInfo.ContentType = "text-plain; charset=utf-8"
	}
	return
}

func (id Id) EndLife() {
	pastes.Lock()
	defer pastes.Unlock()
	err := os.Remove(id.Path())
	if err == nil {
		delete(pastes.m, id)
		log.Printf("Removed paste: %s", id)
	} else {
		log.Printf("Could not end the life of %s: %s", id, err)
		id.EndLifeAfter(2 * time.Minute)
	}
}

func (id Id) EndLifeAfter(duration time.Duration) {
	timer := time.NewTimer(duration)
	go func() {
		<-timer.C
		id.EndLife()
	}()
}

type ByteSize int64

const (
	B ByteSize = 1 << (10 * iota)
	KB
	MB
)

func parseByteSize(str string) (ByteSize, error) {
	if !regexByteSize.MatchString(str) {
		return 0, errors.New("Could not parse size in bytes")
	}
	parts := regexByteSize.FindStringSubmatch(str)
	size, _ := strconv.ParseFloat(string(parts[1]), 64)

	switch string(parts[2]) {
	case "KB", "K":
		size *= float64(KB)
	case "MB", "M":
		size *= float64(MB)
	}
	return ByteSize(size), nil
}

func (b ByteSize) String() string {
	switch {
	case b >= MB:
		return fmt.Sprintf("%.2fMB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2fKB", float64(b)/float64(KB))
	}
	return fmt.Sprintf("%dB", b)
}

func handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		switch r.URL.Path {
		case "/":
			indexTemplate.Execute(w, struct {
				SiteUrl, LifeTime string
			}{
				siteUrl,
				fmt.Sprintf("%g hours and %g minutes", lifeTime.Hours(),
					lifeTime.Minutes()-lifeTime.Hours()*60),
			})
			return
		case "/form":
			formTemplate.Execute(w, struct{ SiteUrl string }{siteUrl})
			return
		}
		id, err := IdFromString(r.URL.Path[1:])
		if err != nil {
			http.Error(w, invalidId, http.StatusBadRequest)
			return
		}
		pastes.RLock()
		defer pastes.RUnlock()
		pasteInfo, e := pastes.m[id]
		if !e {
			http.Error(w, pasteNotFound, http.StatusNotFound)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			if pasteInfo.Etag == inm || inm == "*" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		pasteFile, err := os.Open(id.Path())
		if err != nil {
			http.Error(w, unknownError, http.StatusInternalServerError)
			return
		}
		defer pasteFile.Close()
		w.Header().Set("Etag", pasteInfo.Etag)
		w.Header().Set("Content-Type", pasteInfo.ContentType)
		http.ServeContent(w, r, "", pasteInfo.ModTime, pasteFile)

	case "POST":
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxSize))
		if err := r.ParseMultipartForm(int64(maxSize)); err != nil {
			log.Printf("Could not parse POST multipart form: %s", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var content string
		if vs, found := r.Form["paste"]; found && len(vs[0]) > 0 {
			content = vs[0]
		} else {
			http.Error(w, missingForm, http.StatusBadRequest)
			return
		}
		pastes.Lock()
		defer pastes.Unlock()
		id, err := RandomId()
		if err != nil {
			log.Println(err)
			http.Error(w, unknownError, http.StatusInternalServerError)
			return
		}
		pastePath := id.Path()
		dir, _ := path.Split(pastePath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Printf("Could not create directories leading to %s: %s", pastePath, err)
			http.Error(w, unknownError, http.StatusInternalServerError)
			return
		}
		id.EndLifeAfter(lifeTime)
		modTime := time.Now()
		pasteFile, err := os.OpenFile(pastePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			log.Printf("Could not create new paste file %s: %s", pastePath, err)
			http.Error(w, unknownError, http.StatusInternalServerError)
			return
		}
		defer pasteFile.Close()
		written, err := io.WriteString(pasteFile, content)
		if err != nil {
			log.Printf("Could not write data into %s: %s", pastePath, err)
			http.Error(w, unknownError, http.StatusInternalServerError)
			return
		}
		pasteInfo := id.GenPasteInfo(modTime, []byte(content))
		pastes.m[id] = pasteInfo
		log.Printf("Created new paste %s (%s %s)", id, pasteInfo.ContentType, ByteSize(written))
		fmt.Fprintf(w, "%s/%s\n", siteUrl, id)
	}
}

func walkFunc(filePath string, fileInfo os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if fileInfo.IsDir() {
		return nil
	}
	id, err := IdFromPath(filePath)
	if err != nil {
		return errors.New("Found incompatible id at path " + filePath)
	}
	modTime := fileInfo.ModTime()
	deathTime := modTime.Add(lifeTime)
	now := time.Now()
	if deathTime.Before(now) {
		go id.EndLife()
		return nil
	}
	pasteFile, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer pasteFile.Close()
	read := make([]byte, 512)
	_, err = pasteFile.Read(read)
	if err != nil {
		return err
	}
	var lifeLeft time.Duration
	if deathTime.After(now.Add(lifeTime)) {
		lifeLeft = lifeTime
	} else {
		lifeLeft = deathTime.Sub(now)
	}
	pasteInfo := id.GenPasteInfo(modTime, read)
	pastes.m[id] = pasteInfo
	log.Printf("Recovered paste %s (%s %s) from %s has %s left",
		id, pasteInfo.ContentType, ByteSize(fileInfo.Size()), pasteInfo.ModTime, lifeLeft)
	id.EndLifeAfter(lifeLeft)
	return nil
}

func main() {
	var err error
	flag.Parse()
	if maxSize, err = parseByteSize(maxSizeStr); err != nil {
		log.Fatalf("Invalid max size '%s': %s", maxSizeStr, err)
	}
	if indexTemplate, err = template.ParseFiles(indexTmpl); err != nil {
		log.Fatalf("Could not load template %s: %s", indexTmpl, err)
	}
	if formTemplate, err = template.ParseFiles(formTmpl); err != nil {
		log.Fatalf("Could not load template %s: %s", formTmpl, err)
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatalf("Could not create data directory %s: %s", dataDir, err)
	}
	if err = os.Chdir(dataDir); err != nil {
		log.Fatalf("Could not enter data directory %s: %s", dataDir, err)
	}
	log.Printf("maxSize  = %s", maxSize)
	log.Printf("siteUrl  = %s", siteUrl)
	log.Printf("listen   = %s", listen)
	log.Printf("dataDir  = %s", dataDir)
	log.Printf("lifeTime = %s", lifeTime)
	if err = filepath.Walk(".", walkFunc); err != nil {
		log.Fatalf("Could not recover data directory %s: %s", dataDir, err)
	}
	http.HandleFunc("/", handler)
	log.Printf("Up and running!")
	log.Fatal(http.ListenAndServe(listen, nil))
}
