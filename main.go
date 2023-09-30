package main

import (
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/boltdb/bolt"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gorilla/mux"
	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
}

func LoadConf() {
	if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
		log.Fatal().Err(err).Msg("unable to parse config.toml")
	}
}

func NameGen() string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ123456789"
	ll := len(chars)
	b := make([]byte, conf.FileLen)
	rand.Read(b) // generates len(b) random bytes
	for i := int64(0); i < int64(conf.FileLen); i++ {
		b[i] = chars[int(b[i])%ll]
	}
	return string(b)
}

func CheckFile(name string) bool { // false if doesn't exist, true if exists
	tfd, err := os.Open(conf.FileFolder + "/" + name)
	if err != nil {
		return false
	}
	tfd.Close()
	return true
}

func UploadHandler(w http.ResponseWriter, r *http.Request) {
	// expiry sanitize
	twentyfour := int64(86400)

	file, _, err := r.FormFile("file")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer file.Close()

	mtype, err := mimetype.DetectReader(file)
	if err != nil {
		w.Write([]byte("error detecting the mime type of your file\n"))
		return
	}
	file.Seek(0, 0)

	// generate + check name
	var name string
	for {
		id := NameGen()
		name = id + mtype.Extension()
		if !CheckFile(name) {
			break
		}
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("expiry"))
		err := b.Put([]byte(name), []byte(strconv.FormatInt(time.Now().Unix()+twentyfour, 10)))
		return err
	})
	if err != nil {
		log.Error().Err(err).Msg("Failed to put expiry")
	}

	log.Info().Int64("expiry", twentyfour).Msg("Writing new file")

	f, err := os.OpenFile(conf.FileFolder+"/"+name, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		log.Error().Err(err).Msg("Error opening a file for write")
		w.WriteHeader(http.StatusInternalServerError) // change to json
		return
	}
	defer f.Close()

	io.Copy(f, file)

	w.Write([]byte("https://" + conf.VHost + "/uploads/" + name))
}

func Cull() {
	for {
		removed := 0
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
					os.Remove(conf.FileFolder + "/" + string(k))
					removed += 1
					c.Delete()
				}
			}
			return nil
		})
		if removed >= 1 {
			log.Info().Int("amount", removed).Msg("expired")
		}
		time.Sleep(5 * time.Second)
	}
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	LoadConf()

	err := landlock.V2.BestEffort().RestrictPaths(
		landlock.RWDirs("./"+conf.FileFolder),
		landlock.RWFiles(conf.DBFile),
		landlock.RWFiles(conf.Webroot+"/index.html"),
	)

	if err != nil {
		log.Warn().Err(err).Msg("could not landlock")
	}

	_, err = os.Open("/etc/passwd")
	if err == nil {
		log.Warn().Msg("landlock failed, could open /etc/passwd")
	} else {
		log.Info().Err(err).Msg("Landlocked")
	}

	db, err = bolt.Open(conf.DBFile, 0600, nil)
	if err != nil {
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
	r.HandleFunc("/uploads/{name}", func(w http.ResponseWriter, r *http.Request) { // upload hits
		vars := mux.Vars(r)
		if !CheckFile(vars["name"]) {
			w.WriteHeader(http.StatusNotFound)
		} else {
			http.ServeFile(w, r, conf.FileFolder+"/"+vars["name"])
		}
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/index.html")
	}).Methods("GET")
	http.Handle("/", r)

	go Cull()

	serv := &http.Server{
		Addr:     ":" + conf.LPort,
		Handler:  r,
		ErrorLog: nil,
		//ReadTimeout:  20 * time.Second,
		//WriteTimeout: 20 * time.Second,
		IdleTimeout: 20 * time.Second,
	}

	log.Info().Err(err).Msg("listening...")

	if err := serv.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}

	db.Close()
}
