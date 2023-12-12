package main

import (
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
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
	db   *bolt.DB
	conf Config
)

type Config struct {
	Webroot           string `toml:"webroot"`
	LPort             string `toml:"lport"`
	VHost             string `toml:"vhost"`
	DBFile            string `toml:"dbfile"`
	FileLen           int    `toml:"filelen"`
	FileFolder        string `toml:"folder"`
	FileExpirySeconds int    `toml:"fileexpiry"`
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
	err = Scramble(path, size)
	if err != nil {
		return err
	}

	err = Zeros(path, size)
	if err != nil {
		return err
	}

	err = os.Remove(path)
	if err != nil {
		return err
	}

	return nil
}

func Scramble(path string, size int64) error {
	var i int64
	for i = 0; i < 7; i++ { // 7 iterations
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()

		offset, err := file.Seek(0, 0)
		if err != nil {
			return err
		}
		buff := make([]byte, size)
		rand.Read(buff)
		file.WriteAt(buff, offset)
		file.Close()
	}
	return nil
}

func Zeros(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	offset, err := file.Seek(0, 0)
	if err != nil {
		return err
	}
	buff := make([]byte, size)
	file.WriteAt(buff, offset)
	return nil
}

func NameGen(fileNameLength int) string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ1234567890-_"
	ll := len(chars)
	b := make([]byte, fileNameLength)
	rand.Read(b) // generates len(b) random bytes
	for i := int64(0); i < int64(fileNameLength); i++ {
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
	var name string
	var expiryTime int64
	var fileNameLength int

	fileNameLength = 0
	expiryTime = 0

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

	// Check if expiry time is present and length is too long
	if r.PostFormValue("expiry") != "" {
		expiryTime, err = strconv.ParseInt(r.PostFormValue("expiry"), 10, 64)
		if err != nil {
			log.Error().Err(err).Msg("expiry could not be parsed")
		} else {
			// 5 days max
			if expiryTime < 1 || expiryTime > 432000 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	// Default to conf if not present
	if expiryTime == 0 {
		expiryTime = int64(conf.FileExpirySeconds)
	}

	// Check if the file length parameter exists and also if it's too long
	if r.PostFormValue("url_len") != "" {
		fileNameLength, err = strconv.Atoi(r.PostFormValue("url_len"))
		if err != nil {
			log.Error().Err(err).Msg("url_len could not be parsed")
		} else {
			// if the length is < 3 and > 128 return error
			if fileNameLength < 3 || fileNameLength > 128 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	// Default to conf if not present
	if fileNameLength == 0 {
		fileNameLength = conf.FileLen
	}

	// generate + check name
	for {
		id := NameGen(fileNameLength)
		name = id + mtype.Extension()
		if !CheckFile(name) {
			break
		}
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("expiry"))
		err := b.Put([]byte(name), []byte(strconv.FormatInt(time.Now().Unix()+expiryTime, 10)))
		return err
	})
	if err != nil {
		log.Error().Err(err).Msg("Failed to put expiry")
	}

	log.Info().Int64("expiry", expiryTime).Msg("Writing new file")

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
					if err := Shred(conf.FileFolder + "/" + string(k)); err != nil {
						log.Error().Err(err).Msg("shredding failed")
					} else {
						removed += 1
					}
					c.Delete()
				}
			}
			return nil
		})
		if removed >= 1 {
			log.Info().Int("amount", removed).Msg("shredded")
		}
		time.Sleep(5 * time.Second)
	}
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	LoadConf()

	err := landlock.V2.BestEffort().RestrictPaths(
		landlock.RWDirs(conf.FileFolder),
		landlock.RWDirs(conf.Webroot),
		landlock.RWFiles(conf.DBFile),
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
	r.HandleFunc("/uploads/{name}", func(w http.ResponseWriter, r *http.Request) {
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
	r.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/index.html")
	}).Methods("GET")
	r.HandleFunc("/fist.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/fist.ico")
	}).Methods("GET")
	r.HandleFunc("/header.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, conf.Webroot+"/header.png")
	}).Methods("GET")
	http.Handle("/", r)

	go Cull()

	serv := &http.Server{
		Addr:        ":" + conf.LPort,
		Handler:     r,
		ErrorLog:    nil,
		IdleTimeout: 20 * time.Second,
	}

	log.Info().Err(err).Msg("listening on port " + conf.LPort + "...")

	if err := serv.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}

	db.Close()
}
