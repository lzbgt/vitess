// Copyright 2015, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simpleacl

import "github.com/youtube/vitess/go/vt/tableacl/acl"

var allAcl SimpleAcl

const all = "*"

// SimpleAcl keeps all entries in a unique in-memory list
type SimpleAcl map[string]bool

// IsMember checks the membership of a principal in this ACL
func (sacl SimpleAcl) IsMember(principal string) bool {
	return sacl[principal] || sacl[all]
}

// Factory is responsible to create new ACL instance.
type Factory struct{}

// New creates a new ACL instance.
func (factory *Factory) New(entries []string) (acl.ACL, error) {
	acl := SimpleAcl(map[string]bool{})
	for _, e := range entries {
		acl[e] = true
	}
	return acl, nil
}

// All returns an ACL instance that contains all users.
func (factory *Factory) All() acl.ACL {
	if allAcl == nil {
		allAcl = SimpleAcl(map[string]bool{all: true})
	}
	return allAcl
}

// AllString returns a string representation of all users.
func (factory *Factory) AllString() string {
	return all
}

// make sure SimpleAcl implements interface acl.ACL
var _ (acl.ACL) = (*SimpleAcl)(nil)

// make sure Factory implements interface acl.AclFactory
var _ (acl.Factory) = (*Factory)(nil)
