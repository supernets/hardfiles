package main

import (
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
  "compress/gzip"
  "strings"
  "bytes"

	"github.com/BurntSushi/toml"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gorilla/mux"
	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

var (
	db   *bolt.DB
	conf Config
)

type Config struct {
	Webroot    string `toml:"webroot"`
	LPort      string `toml:"lport"`
	VHost      string `toml:"vhost"`
	DBFile     string `toml:"dbfile"`
	FileLen    int    `toml:"filelen"`
	FileFolder string `toml:"folder"`
	DefaultTTL int    `toml:"default_ttl"`
	MaxTTL     int    `toml:"maximum_ttl"`
}

func LoadConf() {
	if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
		log.Fatal().Err(err).Msg("unable to parse config.toml")
	}
}

func Shred(path string) error {
	fileinfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fileinfo.Size()
	if err = Scramble(path, size); err != nil {
		return err
	}

	if err = Zeros(path, size); err != nil {
		return err
	}

	if err = os.Remove(path); err != nil {
		return err
	}

	return nil
}

func Scramble(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	for i := 0; i < 7; i++ { // 7 iterations
		buff := make([]byte, size)
		if _, err := rand.Read(buff); err != nil {
			return err
		}
		if _, err := file.WriteAt(buff, 0); err != nil {
			return err
		}
	}
	return nil
}

func Zeros(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	buff := make([]byte, size)
	file.WriteAt(buff, 0)
	return nil
}

func NameGen() string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ0123456789"
	ll := len(chars)
	b := make([]byte, conf.FileLen)
	rand.Read(b) // generates len(b) random bytes
	for i := int64(0); i < int64(conf.FileLen); i++ {
		b[i] = chars[int(b[i])%ll]
	}
	return string(b)
}

func Exists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func isValidGzipHeader(data []byte) bool {
    return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func UploadHandler(w http.ResponseWriter, r *http.Request) {
	// expiry time
	var ttl int64

	ttl = 0

	var file, header, err = r.FormFile("file")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
  defer file.Close()

  var buffer bytes.Buffer
  _, err = io.Copy(&buffer, file)
  if err != nil {
    log.Error().Err(err).Msg("error reading file data")
    w.WriteHeader(http.StatusInternalServerError)
    return
  }

  var data bytes.Buffer
  var name = header.Filename
  println("file: " + name)
  if strings.HasSuffix(name, ".5000") {
    buffer.WriteTo(&data)
    if isValidGzipHeader(data.Bytes()[:2]) {
      gz, err := gzip.NewReader(&data)
        if err != nil {
          log.Error().Err(err).Msg("error creating gzip reader")
          w.WriteHeader(http.StatusInternalServerError)
          return
        }
        defer gz.Close()

        _, err = io.Copy(&buffer, gz)
        if err != nil {
          log.Error().Err(err).Msg("error decompressing gzip file")
          w.WriteHeader(http.StatusInternalServerError)
          return
        }

        name = strings.TrimSuffix(name, ".5000")
        data.Write(buffer.Bytes())
    } else {
      log.Error().Msg("Invalid gzip file")
      w.WriteHeader(http.StatusBadRequest)
      return
      }
  } else {
    data.Write(buffer.Bytes())
  }

  mtype, err := mimetype.DetectReader(&data)
  if err != nil {
    log.Error().Err(err).Msg("error detecting MIME type")
    w.WriteHeader(http.StatusInternalServerError)
    return
  }

	// Check if expiry time is present and length is too long
	if r.PostFormValue("expiry") != "" {
		ttl, err = strconv.ParseInt(r.PostFormValue("expiry"), 10, 64)
		if err != nil {
			log.Error().Err(err).Msg("expiry could not be parsed")
      w.WriteHeader(http.StatusBadRequest)
		} else {
			// Get maximum ttl length from config and kill upload if specified ttl is too long, this can probably be handled better in the future
			if ttl < 1 || ttl > int64(conf.MaxTTL) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

  // Default to conf if not present
	if ttl == 0 {
		ttl = int64(conf.DefaultTTL)
	}

	// generate + check name
	for {
		id := NameGen()
		name = id + mtype.Extension()
		if !Exists(conf.FileFolder + "/" + name) {
			break
		}
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("expiry"))
		err := b.Put([]byte(name), []byte(strconv.FormatInt(time.Now().Unix()+ttl, 10)))
		return err
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to put expiry")
	}

	f, err := os.OpenFile(conf.FileFolder+"/"+name, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
  if err != nil {
		log.Error().Err(err).Msg("error opening a file for write")
		w.WriteHeader(http.StatusInternalServerError) // change to json
		return
	}
	defer f.Close()

  io.Copy(f, &buffer)
  if err != nil {
    log.Error().Err(err).Msg("error copying file")
    w.WriteHeader(http.StatusInternalServerError)
    return
  }
  buffer.Reset()
  data.Reset()
	log.Info().Str("name", name).Int64("ttl", ttl).Msg("wrote new file")

	hostedurl := "https://" + conf.VHost + "/uploads/" + name

	w.Header().Set("Location", hostedurl)
	w.WriteHeader(http.StatusSeeOther)
	w.Write([]byte(hostedurl))
}

func Cull() {
	for {
		db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("expiry"))
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				eol, err := strconv.ParseInt(string(v), 10, 64)
				if err != nil {
					log.Error().Err(err).Bytes("k", k).Bytes("v", v).Msg("expiration time could not be parsed")
					continue
				}
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
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	LoadConf()

	var err error
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
	if err = landlock.V2.BestEffort().RestrictPaths(
		landlock.RWDirs(conf.FileFolder),
		landlock.RWDirs(conf.Webroot),
		landlock.RWFiles(conf.DBFile),
	); err != nil {
		log.Warn().Err(err).Msg("could not landlock")
	}

	if _, err = os.Open("/etc/passwd"); err != nil {
		log.Warn().Msg("landlock failed, could open /etc/passwd, are you on a 5.13+ kernel?")
	} else {
		log.Info().Err(err).Msg("landlocked")
	}

	if db, err = bolt.Open(conf.DBFile, 0600, nil); err != nil {
		log.Fatal().Err(err).Msg("unable to open database file")
	}
	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("expiry"))
		if err != nil {
			log.Fatal().Err(err).Msg("error creating expiry bucket")
			return err
		}
		return nil
	})

	r := mux.NewRouter()
	r.HandleFunc("/", UploadHandler).Methods("POST")
	r.HandleFunc("/uploads/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if !Exists(conf.FileFolder + "/" + vars["name"]) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("file not found"))
		} else {
			http.ServeFile(w, r, conf.FileFolder+"/"+vars["name"])
		}
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/index.html")
	})
	r.HandleFunc("/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := mux.Vars(r)["file"]
		if _, err := os.Stat(conf.Webroot + "/" + file); os.IsNotExist(err) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			http.ServeFile(w, r, conf.Webroot+"/"+file)
		}
	}).Methods("GET")
	http.Handle("/", r)

	go Cull()

	serv := &http.Server{
		Addr:        ":" + conf.LPort,
		Handler:     r,
		ErrorLog:    nil,
		IdleTimeout: 20 * time.Second,
	}

	log.Warn().Msg("shredding is only effective on HDD volumes")
	log.Info().Err(err).Msg("listening on port " + conf.LPort + "...")

	if err := serv.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}

	db.Close()
}
