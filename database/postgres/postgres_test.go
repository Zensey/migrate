package postgres

// error codes https://github.com/lib/pq/blob/master/error.go

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	nurl "net/url"
	"testing"

	"github.com/lib/pq"
	dt "github.com/mattes/migrate/database/testing"
	mt "github.com/mattes/migrate/testing"
)

var versions = []string{
	"postgres:9.6",
	"postgres:9.5",
	"postgres:9.4",
	"postgres:9.3",
	"postgres:9.2",
}

func isReady(i mt.Instance) bool {
	db, err := sql.Open("postgres", fmt.Sprintf("postgres://postgres@%v:%v/postgres?sslmode=disable", i.Host(), i.Port()))
	if err != nil {
		return false
	}
	defer db.Close()
	err = db.Ping()
	if err == io.EOF {
		return false

	} else if e, ok := err.(*pq.Error); ok {
		if e.Code.Name() == "cannot_connect_now" {
			return false
		}
	}

	return true
}

func Test(t *testing.T) {
	mt.ParallelTest(t, versions, isReady,
		func(t *testing.T, i mt.Instance) {
			p := &Postgres{}
			addr := fmt.Sprintf("postgres://postgres@%v:%v/postgres?sslmode=disable", i.Host(), i.Port())
			d, err := p.Open(addr)
			if err != nil {
				t.Fatalf("%v", err)
			}
			dt.Test(t, d, []byte("SELECT 1"))
		})
}

func TestWithSchema(t *testing.T) {
	mt.ParallelTest(t, versions, isReady,
		func(t *testing.T, i mt.Instance) {
			p := &Postgres{}
			addr := fmt.Sprintf("postgres://postgres@%v:%v/postgres?sslmode=disable", i.Host(), i.Port())
			d, err := p.Open(addr)
			if err != nil {
				t.Fatalf("%v", err)
			}

			// create foobar schema
			if err := d.Run(100, bytes.NewReader([]byte("CREATE SCHEMA foobar AUTHORIZATION postgres"))); err != nil {
				t.Fatal(err)
			}

			// re-connect using that schema
			d2, err := p.Open(fmt.Sprintf("postgres://postgres@%v:%v/postgres?sslmode=disable&search_path=foobar", i.Host(), i.Port()))
			if err != nil {
				t.Fatalf("%v", err)
			}

			version, err := d2.Version()
			if err != nil {
				t.Fatal(err)
			}
			if version != -1 {
				t.Fatal("expected NilVersion")
			}

			// now update version and compare
			if err := d2.Run(2, nil); err != nil {
				t.Fatal(err)
			}
			version, err = d2.Version()
			if err != nil {
				t.Fatal(err)
			}
			if version != 2 {
				t.Fatal("expected version 2")
			}

			// meanwhile, the public schema still has the other version
			version, err = d.Version()
			if err != nil {
				t.Fatal(err)
			}
			if version != 100 {
				t.Fatal("expected version 2")
			}
		})
}

func TestWithInstance(t *testing.T) {

}

func TestGenerateAdvisoryLockId(t *testing.T) {
	p := &Postgres{}

	if _, err := p.generateAdvisoryLockId(); err == nil {
		t.Errorf("expected err not to be nil")
	}

	p.url = &nurl.URL{Path: "database_name"}
	id, err := p.generateAdvisoryLockId()
	if err != nil {
		t.Errorf("expected err to be nil, got %v", err)
	}
	if len(id) == 0 {
		t.Errorf("expected generated id not to be empty")
	}
	t.Logf("generated id: %v", id)
}
