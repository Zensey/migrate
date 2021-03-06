package stub

import (
	"io"
	"io/ioutil"
	"reflect"

	"github.com/mattes/migrate/database"
)

func init() {
	database.Register("stub", &Stub{})
}

type Stub struct {
	Url               string
	Instance          interface{}
	CurrentVersion    int
	MigrationSequence []string
	LastRunMigration  []byte // todo: make []string
	IsLocked          bool

	Config *Config
}

func (s *Stub) Open(url string) (database.Driver, error) {
	return &Stub{
		Url:               url,
		CurrentVersion:    -1,
		MigrationSequence: make([]string, 0),
		Config:            &Config{},
	}, nil
}

type Config struct{}

func WithInstance(instance interface{}, config *Config) (database.Driver, error) {
	return &Stub{
		Instance:          instance,
		CurrentVersion:    -1,
		MigrationSequence: make([]string, 0),
		Config:            config,
	}, nil
}

func (s *Stub) Close() error {
	return nil
}

func (s *Stub) Lock() error {
	if s.IsLocked {
		return database.ErrLocked
	}
	s.IsLocked = true
	return nil
}

func (s *Stub) Unlock() error {
	s.IsLocked = false
	return nil
}

func (s *Stub) Run(version int, migration io.Reader) error {
	s.CurrentVersion = version

	if migration != nil {
		m, err := ioutil.ReadAll(migration)
		if err != nil {
			return err
		}
		s.LastRunMigration = m
		s.MigrationSequence = append(s.MigrationSequence, string(m[:]))
	}

	return nil
}

func (s *Stub) Version() (int, error) {
	if s.CurrentVersion < 0 {
		return database.NilVersion, nil
	}
	return s.CurrentVersion, nil
}

const DROP = "DROP"

func (s *Stub) Drop() error {
	s.CurrentVersion = -1
	s.LastRunMigration = nil
	s.MigrationSequence = append(s.MigrationSequence, DROP)
	return nil
}

func (s *Stub) EqualSequence(seq []string) bool {
	return reflect.DeepEqual(seq, s.MigrationSequence)
}
