// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

// The links' writes (S5). Link and Unlink are 0.x's, with 0.x's checks in
// 0.x's order and 0.x's errors, and like Put, Delete and Drop each appends
// the changes it amounts to to dst, a change list, and returns it. Each
// checks everything before it changes anything, so on an error the store is
// as it was and dst comes back as it went in. Delete and Drop take a
// record's links out both ways, through cut. The links' lists are in
// halves.go, and the reads, Neighbours and Walk, in walk.go.

// Link adds a link of the type typ from the record with the key from to
// the one with the key to, such as an "owns" link from customer:42 to
// docs:7. Both records must exist, and a record may link to itself. Adding
// a link that's there already changes nothing and appends nothing, so the
// log writes it once. Otherwise the change is a Link.
//
// The errors are 0.x's, in 0.x's order: one that wraps errs.ErrInvalid for
// a type that breaks the rules (rules.LinkType), then for from, the same
// for a key that breaks them and one that wraps errs.ErrNotFound for a key
// with no record, then the same for to.
func (s *Store) Link(dst []format.Change, from, typ, to string) ([]format.Change, error) {
	s.direct("Link")
	return s.linkKeys(dst, from, typ, to)
}

func (s *Store) linkKeys(dst []format.Change, from, typ, to string) ([]format.Change, error) {
	if err := rules.LinkType(typ); err != nil {
		return dst, err
	}
	a, err := s.existing(from)
	if err != nil {
		return dst, err
	}
	b, err := s.existing(to)
	if err != nil {
		return dst, err
	}
	if _, there := a.out.get(linkAt{typ, b.key}); there && plant != "store/link-added-twice" {
		return dst, nil
	}
	h := s.handle(typ)
	s.link(a, h, b)
	return append(dst, format.Change{Op: format.Link, Key: a.key, Type: h.Value(), To: b.key}), nil
}

// Unlink takes out the link of the type typ from the record with the key
// from to the one with the key to, or every link from one to the other when
// typ is "". The changes are an Unlink for each link it takes out, in byte
// order of type. As in 0.x, it checks neither the keys nor the type: when
// there's no such link, whatever the reason, the error wraps
// errs.ErrNotFound, with 0.x's message.
func (s *Store) Unlink(dst []format.Change, from, typ, to string) ([]format.Change, error) {
	s.direct("Unlink")
	return s.unlinkKeys(dst, from, typ, to)
}

func (s *Store) unlinkKeys(dst []format.Change, from, typ, to string) ([]format.Change, error) {
	a, b := s.records[from], s.records[to]
	var types []linkType
	if a != nil && b != nil {
		if typ == "" {
			types = between(a, b)
		} else if h, there := a.out.get(linkAt{typ, b.key}); there {
			types = append(types, h.typ)
		}
	}
	if len(types) == 0 {
		return dst, fmt.Errorf("%w: no link from %s to %s", errs.ErrNotFound, from, to)
	}
	for _, t := range types {
		s.unlink(a, t, b)
		dst = append(dst, format.Change{Op: format.Unlink, Key: a.key, Type: t.Value(), To: b.key})
	}
	return dst, nil
}

// between returns the types of the links from a to b, in byte order. It
// looks through a's links out or b's links in, whichever is shorter, since
// the links between two records can be of any types.
func between(a, b *record) []linkType {
	l, other := &a.out, b
	if b.in.len() < a.out.len() {
		l, other = &b.in, a
	}
	var types []linkType
	for h := range l.ofType("") {
		if h.other == other {
			types = append(types, h.typ)
		}
	}
	if plant == "store/unlink-types-reversed" {
		for i, j := 0, len(types)-1; i < j; i, j = i+1, j-1 {
			types[i], types[j] = types[j], types[i]
		}
	}
	return types
}

// applyLink makes a Link change, by FORMAT.md's rules: the type keeps the
// rules, both records exist, and the link isn't there already.
func (s *Store) applyLink(c format.Change) error {
	if err := rules.LinkType(c.Type); err != nil {
		return err
	}
	a, err := s.existing(c.Key)
	if err != nil {
		return err
	}
	b, err := s.existing(c.To)
	if err != nil {
		return err
	}
	if _, there := a.out.get(linkAt{c.Type, b.key}); there {
		return fmt.Errorf("%w: the link %v is added when it's there already", errs.ErrInvalid, Link{c.Key, c.Type, c.To})
	}
	s.link(a, s.handle(c.Type), b)
	return nil
}

// applyUnlink makes an Unlink change, by FORMAT.md's rules: the link is
// there. Its type and keys keep the rules, as the codec requires.
func (s *Store) applyUnlink(c format.Change) error {
	if err := rules.LinkType(c.Type); err != nil {
		return err
	}
	if _, err := rules.TableOf(c.Key); err != nil {
		return err
	}
	if _, err := rules.TableOf(c.To); err != nil {
		return err
	}
	a, b := s.records[c.Key], s.records[c.To]
	var h half
	there := false
	if a != nil && b != nil {
		h, there = a.out.get(linkAt{c.Type, b.key})
	}
	if !there {
		return fmt.Errorf("%w: no link %v to remove", errs.ErrNotFound, Link{c.Key, c.Type, c.To})
	}
	s.unlink(a, h.typ, b)
	return nil
}

// handle returns the handle of the link type typ, which every link of that
// type shares, so its text is kept once.
func (s *Store) handle(typ string) linkType {
	if s.lastType == (linkType{}) || s.lastType.Value() != typ {
		s.lastType = makeType(typ)
	}
	return s.lastType
}

// The small functions that change the links, each with its undo entry
// first, as changing requires.

// link adds the link of the type typ from a to b, which isn't there.
func (s *Store) link(a *record, typ linkType, b *record) {
	s.changing(undo{op: undoNoLink, record: a, half: half{typ, b}})
	a.out.insert(half{typ, b})
	b.in.insert(half{typ, a})
}

// unlink takes out the link of the type typ from a to b, which is there.
func (s *Store) unlink(a *record, typ linkType, b *record) {
	s.changing(undo{op: undoHadLink, record: a, half: half{typ, b}})
	a.out.remove(linkAt{typ.Value(), b.key})
	if plant != "store/unlink-one-end" {
		b.in.remove(linkAt{typ.Value(), a.key})
	}
}

// cutHalf takes the half h out of r's links out when out is true, or out of
// its links in, for a delete or a drop of the record at h's other end.
func (s *Store) cutHalf(r *record, out bool, h half) {
	if out {
		s.changing(undo{op: undoHadOut, record: r, half: h})
		r.out.remove(h.at())
		return
	}
	s.changing(undo{op: undoHadIn, record: r, half: h})
	r.in.remove(h.at())
}

// cut takes r's links out at their other ends, for a delete of r or a drop
// of r's table: every half that points at r in the list of a record that
// stays. That's every record but r itself for a delete, when gone is nil,
// and every record outside gone, r's table, for a drop. r's own lists, and
// those of the other records a drop takes, stay as they are, since nothing
// can reach those records once they're out of the hash table, and undoing
// the delete or the drop puts them back as they were.
func (s *Store) cut(r *record, gone *table) {
	for h := range r.out.ofType("") {
		if h.other != r && h.other.table != gone {
			s.cutHalf(h.other, false, half{h.typ, r})
		}
	}
	if plant == "store/links-in-kept" {
		return
	}
	for h := range r.in.ofType("") {
		if h.other != r && h.other.table != gone {
			s.cutHalf(h.other, true, half{h.typ, r})
		}
	}
}
