/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package store

import (
	"fmt"
	"net/url"
	"strings"
)

// NewFromURI dispatches to a Store implementation based on the URI scheme.
// Supported forms:
//
//	sqlite:/absolute/path.db                           → file-backed SQLite
//	sqlite:///absolute/path.db                         → same, URL form
//	sqlite:file:/path/to/db?cache=shared               → raw SQLite driver DSN
//	mysql://user:pass@host:3306/db?param=value         → MySQL via go-sql-driver/mysql
//	memory://                                          → in-process in-memory SQLite (test/throwaway)
//
// The "sqlite:" prefix is stripped and the remainder is forwarded to the
// SQLite driver verbatim, so any DSN form the driver accepts (file paths,
// URI-form options) works without bespoke parsing. For in-memory storage
// use memory:// — production deployments should use sqlite: or mysql://.
//
// secretKey is forwarded to backends that encrypt stored secrets at rest.
func NewFromURI(uri, secretKey string) (Store, error) {
	if uri == "" {
		uri = "sqlite:/tmp/aibrix-console.db"
	}
	if strings.HasPrefix(uri, "sqlite:") {
		dsn := strings.TrimPrefix(uri, "sqlite:")
		// sqlite:///abs/path → //abs/path → /abs/path so the URL form and
		// the raw driver form converge on the same DSN.
		dsn = strings.TrimPrefix(dsn, "//")
		return NewSQLiteStore(dsn, secretKey)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse store URI %q: %w", uri, err)
	}
	switch u.Scheme {
	case "memory":
		return NewMemoryStore(), nil
	case "mysql":
		return NewMySQLStore(mysqlURIToDSN(u), secretKey)
	default:
		return nil, fmt.Errorf("unsupported store scheme %q (URI %q); use sqlite:, mysql://, or memory://", u.Scheme, uri)
	}
}

// mysqlURIToDSN converts a `mysql://user:pass@host:3306/db?param=value` URL
// into the DSN format the go-sql-driver/mysql driver expects:
//
//	user:pass@tcp(host:3306)/db?param=value
//
// Both URI and DSN forms percent-decode credentials the same way; we reuse
// url.URL.User to handle that. If the host has no port we leave it intact —
// callers can still supply ?addr=... or use the default port via tcp().
func mysqlURIToDSN(u *url.URL) string {
	creds := ""
	if u.User != nil {
		creds = u.User.String() + "@"
	}
	host := u.Host
	dbname := ""
	if len(u.Path) > 1 {
		dbname = u.Path[1:] // strip leading "/"
	}
	dsn := fmt.Sprintf("%stcp(%s)/%s", creds, host, dbname)
	if u.RawQuery != "" {
		dsn += "?" + u.RawQuery
	}
	return dsn
}
