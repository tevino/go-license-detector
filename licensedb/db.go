package licensedb

import (
	"archive/tar"
	"bytes"
	"encoding/csv"
	"index/suffixarray"
	"io"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ekzhu/minhash-lsh"
	"github.com/sergi/go-diff/diffmatchpatch"

	"gopkg.in/src-d/go-license-detector.v1/licensedb/internal/assets"
	"gopkg.in/src-d/go-license-detector.v1/licensedb/internal/normalize"
	"gopkg.in/src-d/go-license-detector.v1/licensedb/internal/wmh"
)

// database holds the license texts, their hashes and the hashtables to query for nearest
// neighbors.
type database struct {
	debug bool

	// license name -> text
	licenseTexts map[string]string
	// minimum license text length
	minLicenseLength int
	// official license URLs
	urls map[string]string
	// all URLs joined
	urlRe *regexp.Regexp
	// first line of each license OR-ed - used to split
	firstLineRe *regexp.Regexp
	// unique unigrams -> index
	tokens map[string]int
	// document frequencies of the unigrams, indexes match with `tokens`
	docfreqs []int
	// Weighted MinHash hashtables
	lsh *minhashlsh.MinhashLSH
	// turns a license text into a hash
	hasher *wmh.WeightedMinHasher
	// part of license name -> list of containing license names
	nameSubstrings map[string][]substring
	// number of substrings per license
	nameSubstringSizes map[string]int
}

type substring struct {
	value string
	count int
}

const (
	numHashes           = 154
	similarityThreshold = 0.75
)

// Length returns the number of registered licenses.
func (db database) Length() int {
	return len(db.licenseTexts)
}

// VocabularySize returns the number of unique unigrams.
func (db database) VocabularySize() int {
	return len(db.tokens)
}

// Load takes the licenses from the embedded storage, normalizes, hashes them and builds the
// LSH hashtables.
func loadLicenses() *database {
	db := &database{}
	if os.Getenv("LICENSE_DEBUG") != "" {
		db.debug = true
	}
	urlCSVBytes, err := assets.Asset("urls.csv")
	if err != nil {
		log.Fatalf("failed to load urls.csv from the assets: %v", err)
	}
	urlReader := csv.NewReader(bytes.NewReader(urlCSVBytes))
	records, err := urlReader.ReadAll()
	if err != nil || len(records) == 0 {
		log.Fatalf("failed to parse urls.csv from the assets: %v", err)
	}
	db.urls = map[string]string{}
	urlReWriter := &bytes.Buffer{}
	for i, record := range records {
		db.urls[record[1]] = record[0]
		urlReWriter.Write([]byte(regexp.QuoteMeta(record[1])))
		if i < len(records)-1 {
			urlReWriter.WriteRune('|')
		}
	}
	db.urlRe = regexp.MustCompile(urlReWriter.String())
	tarBytes, err := assets.Asset("licenses.tar")
	if err != nil {
		log.Fatalf("failed to load licenses.tar from the assets: %v", err)
	}
	tarStream := bytes.NewBuffer(tarBytes)
	archive := tar.NewReader(tarStream)
	db.licenseTexts = map[string]string{}
	tokenFreqs := map[string]map[string]int{}
	firstLineWriter := &bytes.Buffer{}
	firstLineWriter.WriteString("(^|\\n)(")
	for header, err := archive.Next(); err != io.EOF; header, err = archive.Next() {
		if len(header.Name) <= 6 {
			continue
		}
		key := header.Name[2 : len(header.Name)-4]
		text := make([]byte, header.Size)
		readSize, readErr := archive.Read(text)
		if readErr != nil {
			log.Fatalf("failed to load licenses.tar from the assets: %s: %v", header.Name, readErr)
		}
		if int64(readSize) != header.Size {
			log.Fatalf("failed to load licenses.tar from the assets: %s: incomplete read", header.Name)
		}
		normedText := normalize.LicenseText(string(text), normalize.Moderate)
		if db.minLicenseLength == 0 || db.minLicenseLength > len(normedText) {
			db.minLicenseLength = len(normedText)
		}
		db.licenseTexts[key] = normedText
		newLinePos := strings.Index(normedText, "\n")
		if newLinePos >= 0 {
			firstLineWriter.WriteString(regexp.QuoteMeta(normedText[:newLinePos]))
			firstLineWriter.WriteRune('|')
		}
		normedText = normalize.Relax(normedText)
		lines := strings.Split(normedText, "\n")
		myUniqueTokens := map[string]int{}
		tokenFreqs[key] = myUniqueTokens
		for _, line := range lines {
			tokens := strings.Split(line, " ")
			for _, token := range tokens {
				myUniqueTokens[token]++
			}
		}
	}
	if db.debug {
		println("Minimum license length:", db.minLicenseLength)
	}
	firstLineWriter.Truncate(firstLineWriter.Len()-1)
	firstLineWriter.WriteRune(')')
	db.firstLineRe = regexp.MustCompile(firstLineWriter.String())
	docfreqs := map[string]int{}
	for _, tokens := range tokenFreqs {
		for token := range tokens {
			docfreqs[token]++
		}
	}
	uniqueTokens := make([]string, len(docfreqs))
	{
		i := 0
		for token := range docfreqs {
			uniqueTokens[i] = token
			i++
		}
	}
	sort.Strings(uniqueTokens)
	db.tokens = map[string]int{}
	db.docfreqs = make([]int, len(uniqueTokens))
	for i, token := range uniqueTokens {
		db.tokens[token] = i
		db.docfreqs[i] = docfreqs[token]
	}
	db.lsh = minhashlsh.NewMinhashLSH64(numHashes, similarityThreshold)
	if db.debug {
		k, l := db.lsh.Params()
		println("LSH:", k, l)
	}
	db.hasher = wmh.NewWeightedMinHasher(len(uniqueTokens), numHashes, 7)
	db.nameSubstrings = map[string][]substring{}
	db.nameSubstringSizes = map[string]int{}
	for key, tokens := range tokenFreqs {
		indices := make([]int, len(tokens))
		values := make([]float32, len(tokens))
		{
			i := 0
			for t, freq := range tokens {
				indices[i] = db.tokens[t]
				values[i] = tfidf(freq, db.docfreqs[indices[i]], len(db.licenseTexts))
				i++
			}
		}
		db.lsh.Add(key, db.hasher.Hash(values, indices))

		// register all substrings
		parts := splitLicenseName(key)
		db.nameSubstringSizes[key] = len(parts)
		for _, part := range parts {
			list := db.nameSubstrings[part.value]
			if list == nil {
				list = []substring{}
			}
			list = append(list, substring{value: key, count: part.count})
			db.nameSubstrings[part.value] = list
		}
	}
	db.lsh.Index()
	return db
}

// QueryLicenseText returns the most similar registered licenses.
func (db *database) QueryLicenseText(text string) map[string]float32 {
	parts := normalize.Split(text)
	licenses := map[string]float32{}
	for _, part := range parts {
		for key, val := range db.queryAbstract(part) {
			if licenses[key] < val {
				licenses[key] = val
			}
		}
	}
	return licenses
}

func (db *database) queryAbstract(text string) map[string]float32 {
	normalizedModerate := normalize.LicenseText(text, normalize.Moderate)
	titlePositions := db.firstLineRe.FindAllStringIndex(normalizedModerate, -1)
	candidates := db.queryAbstractNormed(normalizedModerate)
	var prevPos int
	var prevMatch string
	for i, titlePos := range titlePositions {
		begPos := titlePos[0]
		match := normalizedModerate[titlePos[0]:titlePos[1]]
		if match[0] == '\n' {
			match = match[1:]
		}
		if match == prevMatch {
			begPos = prevPos
		}
		if normalizedModerate[begPos] == '\n' {
			begPos++
		}
		var endPos int
		if i < len(titlePositions) - 1 {
			endPos = titlePositions[i + 1][0]
		} else {
			endPos = len(normalizedModerate)
		}
		part := normalizedModerate[begPos:endPos]
		if float64(len(part)) < float64(db.minLicenseLength) *similarityThreshold {
			prevMatch = match
			prevPos = begPos
			continue
		}
		newCandidates := db.queryAbstractNormed(part)
		if len(newCandidates) == 0 {
			prevMatch = match
			prevPos = begPos
			continue
		}
		prevMatch = ""
		prevPos = -1
		for key, val := range newCandidates {
			if candidates[key] < val {
				candidates[key] = val
			}
		}
	}
	for key := range db.scanForURLs(text) {
		if _, exists := candidates[key]; !exists {
			candidates[key] = 1
		}
	}
	return candidates
}

func (db *database) queryAbstractNormed(normalizedModerate string) map[string]float32 {
	normalizedRelaxed := normalize.Relax(normalizedModerate)
	if db.debug {
		println("\nqueryAbstractNormed --------\n")
		println(normalizedModerate)
		println("\n========\n")
		println(normalizedRelaxed)
	}
	tokens := map[int]int{}
	for _, line := range strings.Split(normalizedRelaxed, "\n") {
		for _, token := range strings.Split(line, " ") {
			if index, exists := db.tokens[token]; exists {
				tokens[index]++
			}
		}
	}
	indices := make([]int, len(tokens))
	values := make([]float32, len(tokens))
	{
		i := 0
		for key, val := range tokens {
			indices[i] = key
			values[i] = tfidf(val, db.docfreqs[key], len(db.licenseTexts))
			i++
		}
	}
	found := db.lsh.Query(db.hasher.Hash(values, indices))
	candidates := map[string]float32{}
	if len(found) == 0 {
		return candidates
	}
	for _, keyint := range found {
		key := keyint.(string)
		licenseText := db.licenseTexts[key]
		yourRunes := make([]rune, 0, len(licenseText)/6)
		vocabulary := map[string]int{}
		for _, line := range strings.Split(licenseText, "\n") {
			for _, token := range strings.Split(line, " ") {
				index, exists := vocabulary[token]
				if !exists {
					index = len(vocabulary)
					vocabulary[token] = index
				}
				yourRunes = append(yourRunes, rune(index))
			}
		}

		oovRune := rune(len(vocabulary))
		myRunes := make([]rune, 0, len(normalizedModerate)/6)
		for _, line := range strings.Split(normalizedModerate, "\n") {
			for _, token := range strings.Split(line, " ") {
				if index, exists := vocabulary[token]; exists {
					myRunes = append(myRunes, rune(index))
				} else if len(myRunes) == 0 || myRunes[len(myRunes)-1] != oovRune {
					myRunes = append(myRunes, oovRune)
				}
			}
		}

		dmp := diffmatchpatch.New()
		diff := dmp.DiffMainRunes(myRunes, yourRunes, false)

		if db.debug {
			tokarr := make([]string, len(db.tokens)+1)
			for key, val := range db.tokens {
				tokarr[val] = key
			}
			tokarr[len(db.tokens)] = "!"
			println(dmp.DiffPrettyText(dmp.DiffCharsToLines(diff, tokarr)))
		}
		distance := dmp.DiffLevenshtein(diff)
		candidates[key] = float32(1) - float32(distance)/float32(len(myRunes))
	}
	weak := make([]string, 0, len(candidates))
	for key, val := range candidates {
		if val < similarityThreshold {
			weak = append(weak, key)
		}
	}
	if len(weak) < len(candidates) {
		for _, key := range weak {
			delete(candidates, key)
		}
	}
	return candidates
}

func (db *database) scanForURLs(text string) map[string]bool {
	byteText := []byte(text)
	index := suffixarray.New(byteText)
	urlMatches := index.FindAllIndex(db.urlRe, -1)
	licenses := map[string]bool{}
	for _, match := range urlMatches {
		url := string(byteText[match[0]:match[1]])
		licenses[db.urls[url]] = true
	}
	return licenses
}

// QueryReadmeText tries to detect licenses mentioned in the README.
func (db *database) QueryReadmeText(text string) map[string]float32 {
	candidates := investigateReadmeFile(text, db.nameSubstrings, db.nameSubstringSizes)
	for key := range db.scanForURLs(text) {
		if _, exists := candidates[key]; !exists {
			candidates[key] = 1
		}
	}
	return candidates
}

func tfidf(freq int, docfreq int, ndocs int) float32 {
	return float32(math.Log(1+float64(freq)) * math.Log(float64(ndocs)/float64(docfreq)))
}
