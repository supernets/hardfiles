// Package main implements hardfiles, a temporary file hosting service with
// automatic expiration and secure file shredding capabilities.
//
// Features:
//   - Streaming file uploads (memory-efficient for large files)
//   - Automatic MIME type detection
//   - Configurable TTL with automatic cleanup
//   - Secure file shredding (7-pass random overwrite + zero fill)
//   - Optional gzip decompression for .5000 suffixed files
//   - Landlock sandboxing on Linux 5.13+
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gorilla/mux"
	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

var (
	db   *bolt.DB // BoltDB instance for storing file expiry metadata
	conf Config   // Global configuration loaded from config.toml
)

// Config holds all application configuration values loaded from config.toml.
type Config struct {
	Webroot    string `toml:"webroot"`     // Directory containing static web assets
	LPort      string `toml:"lport"`       // Port to listen on
	VHost      string `toml:"vhost"`       // Virtual host for generating URLs
	DBFile     string `toml:"dbfile"`      // Path to BoltDB database file
	FileLen    int    `toml:"filelen"`     // Length of generated file IDs
	FileFolder string `toml:"folder"`      // Directory for storing uploaded files
	DefaultTTL int    `toml:"default_ttl"` // Default time-to-live in seconds
	MaxTTL     int    `toml:"maximum_ttl"` // Maximum allowed TTL in seconds
}

// LoadConf reads and parses the configuration from config.toml.
// Terminates the application if the config file cannot be read or parsed.
func LoadConf() {
	if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
		log.Fatal().Err(err).Msg("unable to parse config.toml")
	}
}

// Shred securely deletes a file by overwriting it with random data,
// then zeros, before finally removing it from the filesystem.
// Note: Only effective on HDD volumes; SSDs may retain data due to wear leveling.
func Shred(path string) error {
	fileinfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fileinfo.Size()

	// Overwrite with random data (7 passes)
	if err = Scramble(path, size); err != nil {
		return err
	}

	// Overwrite with zeros
	if err = Zeros(path, size); err != nil {
		return err
	}

	// Remove the file
	if err = os.Remove(path); err != nil {
		return err
	}

	return nil
}

// Scramble overwrites a file with cryptographically random data.
// Performs 7 iterations as per DoD 5220.22-M standard.
// Uses chunked writes (32KB) to avoid memory issues with large files.
func Scramble(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	const chunkSize = 32 * 1024 // 32KB chunks for memory efficiency
	buff := make([]byte, chunkSize)

	for i := 0; i < 7; i++ {
		var offset int64 = 0
		for offset < size {
			writeSize := chunkSize
			if size-offset < int64(chunkSize) {
				writeSize = int(size - offset)
			}
			if _, err := rand.Read(buff[:writeSize]); err != nil {
				return err
			}
			if _, err := file.WriteAt(buff[:writeSize], offset); err != nil {
				return err
			}
			offset += int64(writeSize)
		}
	}
	return nil
}

// Zeros overwrites a file with null bytes.
// Uses chunked writes (32KB) to avoid memory issues with large files.
func Zeros(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	const chunkSize = 32 * 1024 // 32KB chunks for memory efficiency
	buff := make([]byte, chunkSize)

	var offset int64 = 0
	for offset < size {
		writeSize := chunkSize
		if size-offset < int64(chunkSize) {
			writeSize = int(size - offset)
		}
		if _, err := file.WriteAt(buff[:writeSize], offset); err != nil {
			return err
		}
		offset += int64(writeSize)
	}
	return nil
}

// NameGen generates a random alphanumeric string for use as a file identifier.
// Excludes ambiguous characters (i, l, o, I, L, O) for readability.
// Length is determined by conf.FileLen.
func NameGen() string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ0123456789"
	ll := len(chars)
	b := make([]byte, conf.FileLen)
	if _, err := rand.Read(b); err != nil {
		log.Error().Err(err).Msg("failed to generate random bytes")
	}
	for i := 0; i < conf.FileLen; i++ {
		b[i] = chars[int(b[i])%ll]
	}
	return string(b)
}

// Exists checks if a file or directory exists at the given path.
func Exists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

// isValidGzipHeader checks if the byte slice starts with a valid gzip magic number.
// Gzip files begin with bytes 0x1f 0x8b.
func isValidGzipHeader(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

// UploadHandler handles POST requests to upload files.
// Supports optional gzip decompression for files with .5000 suffix.
// Uses streaming I/O to handle large files without loading them into memory.
//
// Form parameters:
//   - file: The file to upload (required)
//   - expiry: TTL in seconds (optional, defaults to conf.DefaultTTL)
//
// Returns:
//   - 303 See Other with Location header pointing to the uploaded file
//   - 400 Bad Request for invalid input
//   - 500 Internal Server Error for processing failures
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	var ttl int64 = 0

	// Use MultipartReader for true streaming - no memory buffering or temp files
	mr, err := r.MultipartReader()
	if err != nil {
		log.Error().Err(err).Msg("error creating multipart reader")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var file io.ReadCloser
	var originalName string
	var expiryValue string

	// Iterate through parts to find file and expiry fields
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error().Err(err).Msg("error reading multipart part")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		switch part.FormName() {
		case "file":
			file = part
			originalName = part.FileName()
		case "expiry":
			data, _ := io.ReadAll(part)
			expiryValue = string(data)
			part.Close()
		default:
			part.Close()
		}

		if file != nil {
			break // Process file immediately, don't wait for other parts
		}
	}

	if file == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer file.Close()

	isGzipped := strings.HasSuffix(originalName, ".5000")

	// Parse and validate expiry time
	if expiryValue != "" {
		ttl, err = strconv.ParseInt(expiryValue, 10, 64)
		if err != nil {
			log.Error().Err(err).Msg("expiry could not be parsed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if ttl < 1 || ttl > int64(conf.MaxTTL) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	// Use default TTL if not specified
	if ttl == 0 {
		ttl = int64(conf.DefaultTTL)
	}

	// Create temporary file for streaming upload
	tempName := NameGen() + ".tmp"
	tempPath := conf.FileFolder + "/" + tempName

	tempFile, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Error().Err(err).Msg("error creating temp file")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Process upload: decompress if gzipped, otherwise stream directly
	if isGzipped {
		// Validate gzip header before attempting decompression
		gzHeader := make([]byte, 2)
		n, err := io.ReadFull(file, gzHeader)
		if err != nil || n < 2 || !isValidGzipHeader(gzHeader) {
			tempFile.Close()
			os.Remove(tempPath)
			log.Error().Msg("Invalid gzip file")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Reconstruct reader with the header bytes we consumed
		combinedReader := io.MultiReader(bytes.NewReader(gzHeader), file)
		gz, err := gzip.NewReader(combinedReader)
		if err != nil {
			tempFile.Close()
			os.Remove(tempPath)
			log.Error().Err(err).Msg("error creating gzip reader")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Stream decompressed data directly to disk
		_, err = io.Copy(tempFile, gz)
		gz.Close()
		if err != nil {
			tempFile.Close()
			os.Remove(tempPath)
			log.Error().Err(err).Msg("error decompressing gzip file")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else {
		// Stream upload directly to disk (no decompression)
		_, err = io.Copy(tempFile, file)
		if err != nil {
			tempFile.Close()
			os.Remove(tempPath)
			log.Error().Err(err).Msg("error copying file")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	tempFile.Close()

	// Detect MIME type from saved file (reads only first few KB)
	mtype, err := mimetype.DetectFile(tempPath)
	if err != nil {
		os.Remove(tempPath)
		log.Error().Err(err).Msg("error detecting MIME type")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Generate unique filename with appropriate extension
	var name string
	for {
		id := NameGen()
		name = id + mtype.Extension()
		if !Exists(conf.FileFolder + "/" + name) {
			break
		}
	}

	// Atomically move temp file to final location
	finalPath := conf.FileFolder + "/" + name
	if err := os.Rename(tempPath, finalPath); err != nil {
		os.Remove(tempPath)
		log.Error().Err(err).Msg("error renaming temp file")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Record expiry timestamp in database
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("expiry"))
		return b.Put([]byte(name), []byte(strconv.FormatInt(time.Now().Unix()+ttl, 10)))
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to put expiry")
	}

	log.Info().Str("name", name).Int64("ttl", ttl).Msg("wrote new file")

	// Return URL to uploaded file
	hostedurl := "https://" + conf.VHost + "/uploads/" + name
	w.Header().Set("Location", hostedurl)
	w.WriteHeader(http.StatusSeeOther)
	w.Write([]byte(hostedurl))
}

// Cull runs as a background goroutine, periodically checking for and
// securely deleting expired files. Runs every 5 seconds.
func Cull() {
	for {
		db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("expiry"))
			c := b.Cursor()

			for k, v := c.First(); k != nil; k, v = c.Next() {
				// Parse expiry timestamp
				eol, err := strconv.ParseInt(string(v), 10, 64)
				if err != nil {
					log.Error().Err(err).Bytes("k", k).Bytes("v", v).Msg("expiration time could not be parsed")
					continue
				}

				// Check if file has expired
				if time.Now().After(time.Unix(eol, 0)) {
					if err := Shred(conf.FileFolder + "/" + string(k)); err != nil {
						log.Error().Err(err).Msg("shredding failed")
					} else {
						log.Info().Str("name", string(k)).Msg("shredded file")
					}
					c.Delete()
				}
			}
			return nil
		})
		time.Sleep(5 * time.Second)
	}
}

func main() {
	// Initialize structured logging with console output
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	LoadConf()

	var err error

	// Ensure required directories and files exist
	if !Exists(conf.FileFolder) {
		if err = os.Mkdir(conf.FileFolder, 0755); err != nil {
			log.Fatal().Err(err).Msg("unable to create folder")
		}
	}
	if !Exists(conf.DBFile) {
		if _, err = os.Create(conf.DBFile); err != nil {
			log.Fatal().Err(err).Msg("unable to create database file")
		}
	}

	// Apply Landlock sandboxing (Linux 5.13+ only)
	// Restricts filesystem access to only necessary paths
	if err = landlock.V2.BestEffort().RestrictPaths(
		landlock.RWDirs(conf.FileFolder),
		landlock.RWDirs(conf.Webroot),
		landlock.RWFiles(conf.DBFile),
	); err != nil {
		log.Warn().Err(err).Msg("could not landlock")
	}

	// Verify Landlock is working by attempting to access a restricted file
	if f, err := os.Open("/etc/passwd"); err == nil {
		f.Close()
		log.Warn().Msg("landlock failed, could open /etc/passwd, are you on a 5.13+ kernel?")
	} else {
		log.Info().Msg("landlocked")
	}

	// Open BoltDB database
	if db, err = bolt.Open(conf.DBFile, 0600, nil); err != nil {
		log.Fatal().Err(err).Msg("unable to open database file")
	}

	// Initialize expiry bucket
	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("expiry"))
		if err != nil {
			log.Fatal().Err(err).Msg("error creating expiry bucket")
			return err
		}
		return nil
	})

	// Configure HTTP routes
	r := mux.NewRouter()

	// POST / - Upload a file
	r.HandleFunc("/", UploadHandler).Methods("POST")

	// GET /uploads/{name} - Download an uploaded file
	r.HandleFunc("/uploads/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		// Sanitize: prevent path traversal attacks
		if strings.Contains(name, "..") || strings.Contains(name, "/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		filePath := conf.FileFolder + "/" + name
		if !Exists(filePath) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("file not found"))
		} else {
			http.ServeFile(w, r, filePath)
		}
	}).Methods("GET")

	// GET / - Serve the index page
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/index.html")
	})

	// GET /{file} - Serve static assets from webroot
	r.HandleFunc("/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := mux.Vars(r)["file"]
		// Sanitize: prevent path traversal attacks
		if strings.Contains(file, "..") || strings.Contains(file, "/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		filePath := conf.Webroot + "/" + file
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			http.ServeFile(w, r, filePath)
		}
	}).Methods("GET")

	http.Handle("/", r)

	// Start background cleanup goroutine
	go Cull()

	// Configure and start HTTP server
	serv := &http.Server{
		Addr:        ":" + conf.LPort,
		Handler:     r,
		ErrorLog:    nil,
		IdleTimeout: 20 * time.Second,
	}

	log.Warn().Msg("shredding is only effective on HDD volumes")
	log.Info().Msg("listening on port " + conf.LPort + "...")

	if err := serv.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}

	// Note: db.Close() is unreachable since ListenAndServe blocks until fatal error
	// Consider using graceful shutdown with signal handling for proper cleanup
	db.Close()
}
