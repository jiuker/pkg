// Copyright (c) 2015-2022 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package ldap defines the LDAP configuration object and methods used by the
// MinIO server.
package ldap

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
)

const (
	dnDelimiter = ";"
)

// BaseDNInfo contains information about a base DN.
type BaseDNInfo struct {
	// User provided base DN.
	Original string
	// DN string returned by the LDAP server. This value is used as the
	// canonical form of the DN.
	ServerDN string
	// Parsed DN (from `ServerDN` value, not `Original`).
	Parsed *ldap.DN
}

// Config contains configuration to connect to an LDAP server.
type Config struct {
	Enabled bool

	// E.g. "ldap.minio.io:636"
	ServerAddr     string
	SRVRecordName  string
	TLSSkipVerify  bool // allows skipping TLS verification
	ServerInsecure bool // allows plain text connection to LDAP server
	ServerStartTLS bool // allows using StartTLS connection to LDAP server
	RootCAs        *x509.CertPool

	// Lookup bind LDAP service account
	LookupBindDN       string
	LookupBindPassword string

	// User DN search parameters
	UserDNSearchBaseDistName string
	// this is a computed value from UserDNSearchBaseDistName
	UserDNSearchBaseDistNames []BaseDNInfo
	UserDNSearchFilter        string

	// Group search parameters
	GroupSearchBaseDistName string
	// this is a computed value from GroupSearchBaseDistName
	GroupSearchBaseDistNames []BaseDNInfo
	GroupSearchFilter        string
}

// Clone creates a copy of the config.
func (l *Config) Clone() (cloned Config) {
	cloned = *l
	return cloned
}

func (l *Config) connect(ldapAddr string) (ldapConn *ldap.Conn, err error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: l.TLSSkipVerify,
		RootCAs:            l.RootCAs,
	}

	if l.ServerInsecure {
		ldapConn, err = ldap.Dial("tcp", ldapAddr)
	} else {
		if l.ServerStartTLS {
			ldapConn, err = ldap.Dial("tcp", ldapAddr)
		} else {
			ldapConn, err = ldap.DialTLS("tcp", ldapAddr, tlsConfig)
		}
	}

	if ldapConn != nil {
		ldapConn.SetTimeout(30 * time.Second) // Change default timeout to 30 seconds.
		if l.ServerStartTLS {
			err = ldapConn.StartTLS(tlsConfig)
		}
	}

	return ldapConn, err
}

// Connect connect to ldap server.
func (l *Config) Connect() (ldapConn *ldap.Conn, err error) {
	if l == nil || !l.Enabled {
		return nil, errors.New("LDAP is not configured")
	}

	var srvService, srvProto, srvName string
	switch l.SRVRecordName {
	case "on":
		srvName = l.ServerAddr
	case "ldap", "ldaps":
		srvService = l.SRVRecordName
		srvProto = "tcp"
		srvName = l.ServerAddr
	case "":
	default:
		return nil, errors.New("Invalid SRV Record Name parameter")

	}

	if srvName == "" {
		// No SRV Record lookup case.
		ldapAddr := l.ServerAddr

		_, _, err = net.SplitHostPort(ldapAddr)
		if err != nil {
			if strings.Contains(err.Error(), "missing port in address") {
				// Use default LDAP port if none specified "636"
				ldapAddr = net.JoinHostPort(ldapAddr, "636")
			} else {
				return nil, err
			}
		}

		return l.connect(ldapAddr)
	}

	// SRV Record lookup is enabled.
	_, addrs, err := net.LookupSRV(srvService, srvProto, srvName)
	if err != nil {
		return nil, fmt.Errorf("DNS SRV Record lookup error: %w", err)
	}

	var errs []error

	// Return a connection to the first server to which we could connect.
	for _, addr := range addrs {
		ldapAddr := fmt.Sprintf("%s:%d", addr.Target, addr.Port)

		ldapConn, err = l.connect(ldapAddr)
		if err == nil {
			return ldapConn, nil
		}
		errs = append(errs, err)
	}

	// If none of the servers could connect, we all the errors.
	var errMsgs []string
	for i, e := range errs {
		errMsgs = append(errMsgs, fmt.Sprintf("Connect err to %s:%d - %v", addrs[i].Target, addrs[i].Port, e))
	}
	err = fmt.Errorf("Could not connect to any LDAP server: %s", strings.Join(errMsgs, "; "))
	return nil, err
}

// LookupBind connects to LDAP server using the bind user credentials.
func (l *Config) LookupBind(conn *ldap.Conn) error {
	var err error
	if l.LookupBindPassword == "" {
		err = conn.UnauthenticatedBind(l.LookupBindDN)
	} else {
		err = conn.Bind(l.LookupBindDN, l.LookupBindPassword)
	}
	if err != nil {
		if ldap.IsErrorWithCode(err, 49) {
			return fmt.Errorf("LDAP Lookup Bind user invalid credentials error: %w", err)
		}
		return fmt.Errorf("LDAP client: %w", err)
	}
	return nil
}

// LookupUserDN searches for the DN of the user given their username. conn is
// assumed to be using the lookup bind service account.
//
// It is required that the search result in at most one result.
//
// If the user does not exist, an error is returned that starts with:
//
//	"User DN not found for:"
func (l *Config) LookupUserDN(conn *ldap.Conn, username string) (string, error) {
	filter := strings.ReplaceAll(l.UserDNSearchFilter, "%s", ldap.EscapeFilter(username))
	var foundDistNames []string
	for _, userSearchBase := range l.UserDNSearchBaseDistNames {
		searchRequest := ldap.NewSearchRequest(
			userSearchBase.ServerDN,
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
			filter,
			[]string{}, // only need DN, so no pass no attributes here
			nil,
		)

		searchResult, err := conn.Search(searchRequest)
		if err != nil {
			// For a search, if the base DN does not exist, we get a 32 error code.
			// Ref: https://ldap.com/ldap-result-code-reference/
			//
			// This situation is an error because the base DN should exist -
			// it's existence is checked during configuration validation but it
			// is possible that the base DN was deleted after the validation.
			if ldap.IsErrorWithCode(err, 32) {
				return "", fmt.Errorf("Base DN (%s) for user DN search does not exist: %w",
					searchRequest.BaseDN, err)
			}
			return "", err
		}

		for _, entry := range searchResult.Entries {
			normDN, err := NormalizeDN(entry.DN)
			if err != nil {
				return "", err
			}
			foundDistNames = append(foundDistNames, normDN)
		}
	}
	if len(foundDistNames) == 0 {
		return "", fmt.Errorf("User DN not found for: %s", username)
	}
	if len(foundDistNames) != 1 {
		return "", fmt.Errorf("Multiple DNs for %s found - please fix the search filter", username)
	}
	return foundDistNames[0], nil
}

// SearchForUserGroups finds the groups of the user.
func (l *Config) SearchForUserGroups(conn *ldap.Conn, username, bindDN string) ([]string, error) {
	// User groups lookup.
	var groups []string
	if l.GroupSearchFilter != "" {
		for _, groupSearchBase := range l.GroupSearchBaseDistNames {
			filter := strings.ReplaceAll(l.GroupSearchFilter, "%s", ldap.EscapeFilter(username))
			filter = strings.ReplaceAll(filter, "%d", ldap.EscapeFilter(bindDN))
			searchRequest := ldap.NewSearchRequest(
				groupSearchBase.ServerDN,
				ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
				filter,
				nil,
				nil,
			)

			var newGroups []string
			newGroups, err := getGroups(conn, searchRequest)
			if err != nil {
				errRet := fmt.Errorf("Error finding groups of %s: %w", bindDN, err)
				return nil, errRet
			}

			groups = append(groups, newGroups...)
		}
	}

	return groups, nil
}

func getGroups(conn *ldap.Conn, sreq *ldap.SearchRequest) ([]string, error) {
	var groups []string
	sres, err := conn.Search(sreq)
	if err != nil {
		// For a search, if the base DN does not exist, we get a 32 error code.
		// Ref: https://ldap.com/ldap-result-code-reference/
		if ldap.IsErrorWithCode(err, 32) {
			return nil, fmt.Errorf("Base DN (%s) for group search does not exist: %w",
				sreq.BaseDN, err)
		}
		return nil, fmt.Errorf("LDAP client: %w", err)
	}
	for _, entry := range sres.Entries {
		// We only queried one attribute,
		// so we only look up the first one.
		normalizedDN, err := NormalizeDN(entry.DN)
		if err != nil {
			return nil, err
		}
		groups = append(groups, normalizedDN)
	}
	return groups, nil
}

// LookupDN looks up the DN and returns the normalized DN. It only performs a
// base object search to check if the DN exists. If the DN does not exist on the
// server, it returns an empty string and a nil error.
func LookupDN(conn *ldap.Conn, dn string) (string, error) {
	// Check if the DN is valid.
	searchRequest := ldap.NewSearchRequest(
		dn,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)",
		[]string{}, // only need DN, so no pass no attributes here
		nil,
	)

	// This search should return at most one result as it is a base object
	// search.
	searchResult, err := conn.Search(searchRequest)
	if err != nil {
		// For a search, if the base DN does not exist, we get a 32 error code.
		// Ref: https://ldap.com/ldap-result-code-reference/
		//
		// Return no DN and nil error.
		if ldap.IsErrorWithCode(err, 32) {
			return "", nil
		}

		return "", fmt.Errorf("LDAP client: %w", err)
	}

	if len(searchResult.Entries) != 1 {
		return "", fmt.Errorf(
			"Multiple DNs found for %s - this should not happen for a base object search",
			dn)
	}

	foundDistName, err := NormalizeDN(searchResult.Entries[0].DN)
	if err != nil {
		return "", err
	}
	return foundDistName, nil
}

// NormalizeDN normalizes the DN. The ldap library here mainly lowercases the
// attribute type names in the DN.
func NormalizeDN(dn string) (string, error) {
	parsedDN, err := ldap.ParseDN(dn)
	if err != nil {
		return "", fmt.Errorf("DN (%s) parse failure: %w", dn, err)
	}
	return parsedDN.String(), nil
}
