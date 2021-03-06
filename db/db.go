package db

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	COLLECTION_DIR_NAME = "collections"
	DB_VERSION          = 1
)

type Database struct {
	Name            string                 `json:"name"`
	Version         int                    `json:"version"`
	collections     map[string]*Collection `json:"-"`
	collectionMutex sync.RWMutex           `json:"-"`
}

type CustomStructure interface {
	GetDataIndex() []*FullDataIndex
}

func NewDatabase(name string) *Database {
	rand.Seed(time.Now().UnixNano())

	gob.RegisterName("so", &ShardOffset{})
	gob.RegisterName("sh", &ConcurrentMapShared{})
	gob.RegisterName("cl", &Collection{})
	gob.RegisterName("el", &Element{})

	ProfileSystemMemory()

	return &Database{name, DB_VERSION, make(map[string]*Collection), sync.RWMutex{}}
}

func (db *Database) RegisterTypeName(name string, value CustomStructure) {
	gob.RegisterName(name, value)
}

func (db *Database) RegisterType(value CustomStructure) {
	gob.Register(value)
}

// delete redundant data from all of the existing collections
func (db *Database) Optimize() (n int64, err error) {
	db.collectionMutex.Lock()
	defer db.collectionMutex.Unlock()
	n = 0
	for _, c := range db.collections {
		opt, err := c.Optimize()
		if err != nil {
			return 0, err
		}
		n += opt
	}
	return n, err
}

// locate the header file (.shardb)
func (db *Database) LocateDatabase(path string) (string, error) {
	prefix := path
	if path == "" {
		prefix = "./"
	}
	files, err := ioutil.ReadDir(prefix)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".shardb") {
			if path == "" {
				return f.Name(), nil
			}
			return prefix + "\\" + f.Name(), nil
		}
	}
	return "", errors.New("database header not found")
}

// load the database
func (db *Database) ScanAndLoadData(path string) error {
	ln := len(path)
	if ln > 0 && path[len(path)-1] != '\\' {
		path += "\\"
	}

	// Locate the header and compare the version of the database
	headerFilename, err := db.LocateDatabase(path)
	if err != nil {
		return errors.New("failed to locate the header due " + err.Error())
	}
	headerData, err := ioutil.ReadFile(headerFilename)
	if err != nil {
		return errors.New("failed to load the header due " + err.Error())
	}
	header := new(Database)
	err = json.Unmarshal(headerData, &header)
	if err != nil {
		return errors.New("failed to unmarshal the header due " + err.Error())
	}
	// Compare the version now
	vdif := int(math.Abs(float64(db.Version - header.Version)))
	if vdif != 0 {
		// if the version of the file is below the major release, then problems may occur
		if vdif >= 10 {
			return errors.New("old database version")
		}
		log.Println("WARNING! Attempt to load the dataset with a different version", header.Version, "( current", db.Version, ")")
	}

	fullPath := path + COLLECTION_DIR_NAME
	_, err = os.Stat(fullPath)
	if os.IsNotExist(err) {
		return errors.New("collections folder does not exist")
	}

	collections, err := ioutil.ReadDir(fullPath)
	if err != nil {
		return err
	}

	for _, c := range collections {
		if c.IsDir() {
			collectionPath := fullPath + "/" + c.Name()

			collectionFiles, err := ioutil.ReadDir(collectionPath)
			if err != nil {
				return err
			}

			cfLen := len(collectionFiles)
			if cfLen < SHARD_COUNT {
				return errors.New("collection has invalid amount of shards " + strconv.Itoa(cfLen) + ". Expected " + strconv.Itoa(SHARD_COUNT))
			}

			var collection *Collection
			loaded := 0
			files := make([]*os.File, SHARD_COUNT)
			cm := NewConcurrentMap(collectionPath, files)
			cNameExt := c.Name() + ".json.gzip"
			mapIndexLoaded := false

			for _, f := range collectionFiles {
				fName := f.Name()
				if strings.HasPrefix(fName, "shard_") {
					// loading the shard main data
					if strings.HasSuffix(fName, ".gobs") {
						fi, err := os.OpenFile(collectionPath+"/"+fName, os.O_RDWR, os.ModePerm)
						if err != nil {
							return errors.New("collection (" + fName + ") shard (" + fName + ") is unavailable")
						}
						files[loaded] = fi
						// loading the meta
						fName := strings.TrimSuffix(fName, ".gobs") + "_meta.gob.gzip"
						p := NewEncodedCompressedPackage(collectionPath + "/" + fName)
						dec, err := p.LoadDecoder()
						if err != nil {
							return err
						}
						var shard ConcurrentMapShared
						err = dec.Decode(&shard)
						if err != nil {
							return err
						}
						dec = nil
						shard.file = fi
						cm.Shared[shard.Id] = &shard
						loaded++
					}

					// loading the map index
				} else if f.Name() == "map.index" {
					inFile, _ := os.Open(collectionPath + "/" + fName)
					scanner := bufio.NewScanner(inFile)
					scanner.Split(bufio.ScanLines)
					// current map index
					if scanner.Scan() {
						num, err := strconv.ParseUint(scanner.Text(), 10, 64)
						if err != nil {
							return err
						}
						cm.SetCounterIndex(num)
					}
					// sync path
					if scanner.Scan() {
						cm.SyncDestination = path + "/" + scanner.Text()
					}
					inFile.Close()
					mapIndexLoaded = true

					// loading the collection's description
				} else if f.Name() == cNameExt {
					data, err := NewCompressedPackage(collectionPath+"/"+cNameExt, nil).Load()
					if err != nil {
						return err
					}
					collection = new(Collection)
					err = json.Unmarshal(data, collection)
					if err != nil {
						return err
					}

				}
			}

			if !mapIndexLoaded {
				return errors.New("map index file was not loaded")
			}
			if collection == nil {
				return errors.New("collection description file missing")
			}

			collection.Map = cm
			collection.Cache = NewCollectionCache()

			db.collectionMutex.Lock()
			db.collections[c.Name()] = collection
			db.collectionMutex.Unlock()

			if loaded < SHARD_COUNT {
				return errors.New("collection " + c.Name() + " files are corrupted")
			}
		}
	}

	return nil
}

// synchronizes the database with the hard drive
func (db *Database) Sync() error {
	db.collectionMutex.RLock()
	wg := sync.WaitGroup{}
	wg.Add(len(db.collections))
	for _, c := range db.collections {
		go func(cl *Collection) {
			log.Println("Synchronizing " + cl.Name)
			err := cl.Sync()
			if err != nil {
				log.Println("Collection "+cl.Name+" syncronization failed:", err.Error())
			}
			wg.Done()
		}(c)
	}
	db.collectionMutex.RUnlock()

	wg.Wait()

	data, err := json.Marshal(db)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(db.Name+".shardb", data, os.ModePerm)
}

func (db *Database) GetCollectionsCount() int {
	return len(db.collections)
}

func (db *Database) GetTotalObjectsCount() int64 {
	db.collectionMutex.RLock()
	defer db.collectionMutex.RUnlock()

	total := int64(0)
	for _, v := range db.collections {
		total += v.Size()
	}

	return total
}

func (db *Database) GetRandomCollection() (*Collection, error) {
	db.collectionMutex.RLock()
	defer db.collectionMutex.RUnlock()
	ln := len(db.collections)
	if ln <= 0 {
		return nil, errors.New("database has no collections")
	}
	n := rand.Intn(ln)
	i := 0
	for _, v := range db.collections {
		if i == n {
			return v, nil
		}
		i++
	}
	return nil, errors.New("could not pick a collection")
}

func (db *Database) AddCollection(name string) (*Collection, error) {
	if db.GetCollection(name) != nil {
		return nil, errors.New("collection is already exist")
	}

	files := make([]*os.File, SHARD_COUNT)
	path := COLLECTION_DIR_NAME + "/" + name
	os.MkdirAll(path, os.ModePerm)
	for i := 0; i < SHARD_COUNT; i++ {
		f, err := os.Create(path + "/shard_" + strconv.Itoa(i) + ".gobs")
		if err != nil {
			return nil, errors.New("failed to create a shard")
		}
		files[i] = f
	}

	c := NewCollection(path, name, NewConcurrentMap(path, files), make(map[string]*int))
	db.collectionMutex.Lock()
	db.collections[name] = c
	db.collectionMutex.Unlock()

	return c, nil
}

func (db *Database) GetCollection(name string) *Collection {
	db.collectionMutex.RLock()
	c := db.collections[name]
	db.collectionMutex.RUnlock()
	return c
}

func (db *Database) DropCollection(name string) {
	db.collectionMutex.Lock()
	delete(db.collections, name)
	db.collectionMutex.Unlock()
}
