/* Copyright (c) 2014, Daniel Martí <mvdan@mvdan.cc> */
/* See LICENSE for licensing information */

package main

import (
	"compress/zlib"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	idSize    = 8 // should be between 6 and 256
	indexTmpl = "index.html"
	maxSize   = 1 << 20 // whole POST body

	// GET error messages
	invalidId     = "Invalid paste id."
	pasteNotFound = "Paste doesn't exist."
	unknownError  = "Something went terribly wrong."
	// POST error messages
	missingForm = "Paste could not be found inside the posted form."
)

var siteUrl = flag.String("u", "http://localhost:9090", "URL of the site")
var listen = flag.String("l", "localhost:9090", "Host and port to listen to")
var dataDir = flag.String("d", "data", "Directory to store all the pastes in")
var lifeTimeStr = flag.String("t", "12h", "Lifetime of the pastes (units: s,m,h)")
var lifeTime time.Duration

const chars = "abcdefghijklmnopqrstuvwxyz0123456789"

var validId *regexp.Regexp = regexp.MustCompile("^[a-zA-Z0-9]{" + strconv.FormatInt(idSize, 10) + "}$")

var indexTemplate *template.Template

func pathId(id string) string {
	return path.Join(id[0:2], id[2:4], id[4:])
}

const (
	_        = iota
	KB int64 = 1 << (10 * iota)
	MB
)

func readableSize(b int64) string {
	switch {
	case b >= MB:
		return fmt.Sprintf("%.2fMB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2fKB", float64(b)/float64(KB))
	}
	return fmt.Sprintf("%dB", b)
}

func randomId() string {
	s := make([]byte, idSize)
	var offset uint = 0
	for {
		r := rand.Int63()
		for i := 0; i < 8; i++ {
			randbyte := int(r&0xff) % len(chars)
			s[offset] = chars[randbyte]
			offset++
			if offset == idSize {
				return string(s)
			}
			r >>= 8
		}
	}
	return strings.Repeat(chars[0:1], idSize)
}

func endLife(path string) {
	err := os.Remove(path)
	if err == nil {
		log.Printf("Removed paste: %s", path)
	} else {
		log.Printf("Could not end the life of %s: %s", path, err)
		programDeath(path, 2*time.Minute)
	}
}

func programDeath(path string, after time.Duration) {
	timer := time.NewTimer(after)
	go func() {
		<-timer.C
		endLife(path)
	}()
}

func handler(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.Method {
	case "GET":
		var id, pastePath string
		id = r.URL.Path[1:]
		if len(id) == 0 {
			indexTemplate.Execute(w, *siteUrl)
			return
		}
		if !validId.MatchString(id) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "%s\n", invalidId)
			return
		}
		id = strings.ToLower(id)
		pastePath = pathId(id)
		pasteFile, err := os.Open(pastePath)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "%s\n", pasteNotFound)
			return
		}
		compReader, err := zlib.NewReader(pasteFile)
		if err != nil {
			log.Printf("Could not open a compression reader for %s: %s", pastePath, err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s\n", unknownError)
			return
		}
		io.Copy(w, compReader)
		compReader.Close()
		pasteFile.Close()

	case "POST":
		r.Body = http.MaxBytesReader(w, r.Body, maxSize)
		var id, pastePath string
		for {
			id = randomId()
			pastePath = pathId(id)
			if _, err := os.Stat(pastePath); os.IsNotExist(err) {
				break
			}
		}
		if err = r.ParseMultipartForm(maxSize); err != nil {
			log.Printf("Could not parse POST multipart form: %s", err)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "%s\n", err)
			return
		}
		var content string
		if vs, found := r.Form["paste"]; found {
			content = vs[0]
		} else {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "%s\n", missingForm)
			return
		}
		dir, _ := path.Split(pastePath)
		if err = os.MkdirAll(dir, 0700); err != nil {
			log.Printf("Could not create directories leading to %s: %s", pastePath, err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s\n", unknownError)
			return
		}
		programDeath(pastePath, lifeTime)
		pasteFile, err := os.OpenFile(pastePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			log.Printf("Could not create new paste pasteFile %s: %s", pastePath, err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s\n", unknownError)
			return
		}
		compWriter := zlib.NewWriter(pasteFile)
		b, err := io.WriteString(compWriter, content)
		compWriter.Close()
		pasteFile.Close()
		if err != nil {
			log.Printf("Could not write compressed data into %s: %s", pastePath, err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s\n", unknownError)
			return
		}
		log.Printf("Created a new paste: %s (%s)", pastePath, readableSize(int64(b)))
		fmt.Fprintf(w, "%s/%s\n", *siteUrl, id)
	}
}

func walkFunc(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	deathTime := info.ModTime().Add(lifeTime)
	now := time.Now()
	if deathTime.Before(now) {
		go endLife(path)
		return nil
	}
	var lifeLeft time.Duration
	if deathTime.After(now.Add(lifeTime)) {
		lifeLeft = lifeTime
	} else {
		lifeLeft = deathTime.Sub(now)
	}
	log.Printf("Recovered paste %s has %s left", path, lifeLeft)
	programDeath(path, lifeLeft)
	return nil
}

func main() {
	var err error
	flag.Parse()
	if lifeTime, err = time.ParseDuration(*lifeTimeStr); err != nil {
		log.Printf("Invalid lifetime '%s': %s", lifeTimeStr, err)
		return
	}
	if indexTemplate, err = template.ParseFiles(indexTmpl); err != nil {
		log.Printf("Could not load template %s: %s", indexTmpl, err)
		return
	}
	if err = os.MkdirAll(*dataDir, 0700); err != nil {
		log.Printf("Could not create data directory %s: %s", *dataDir, err)
		return
	}
	if err = os.Chdir(*dataDir); err != nil {
		log.Printf("Could not enter data directory %s: %s", *dataDir, err)
		return
	}
	if err = filepath.Walk(".", walkFunc); err != nil {
		log.Printf("Could not recover data directory %s: %s", *dataDir, err)
		return
	}
	log.Printf("idSize   = %d", idSize)
	log.Printf("maxSize  = %s", readableSize(maxSize))
	log.Printf("siteUrl  = %s", *siteUrl)
	log.Printf("listen   = %s", *listen)
	log.Printf("dataDir  = %s", *dataDir)
	log.Printf("lifeTime = %s", lifeTime)
	http.HandleFunc("/", handler)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
