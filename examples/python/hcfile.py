#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
"""Read and write a HyperCrux file with nothing but Python's sqlite3 module.

HyperCrux's rules are SQLite triggers stored in the file, so plain SQL from
any language keeps keys, rows, links and vectors in step. This script shows
the moves: put a record, link two, delete one, run any SQL, and find the
nearest vectors by reading them and comparing them in Python.

    python3 hcfile.py put notes.db docs:7 '{"title": "Q3 plan", "vec": [0.1, 0.8, 0.3]}'
    python3 hcfile.py link notes.db customer:42 owns docs:7
    python3 hcfile.py nearest notes.db docs '[0.1, 0.8, 0.3]' 5
    python3 hcfile.py delete notes.db docs:7
    python3 hcfile.py sql notes.db "SELECT key, title FROM docs"

A table has to exist before Python writes to it, with the columns it needs.
The hypercrux command or the Go package creates them on their first put.
"""

import json
import math
import re
import sqlite3
import struct
import sys

_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,63}$")


def connect(path):
    """Opens the file in autocommit mode: each function below starts its own
    transaction with BEGIN IMMEDIATE, which takes the write lock up front."""
    db = sqlite3.connect(path, timeout=10, isolation_level=None)
    db.execute("PRAGMA busy_timeout = 10000")
    return db


def pack(vec):
    """A vector as HyperCrux stores it: little-endian float32 values."""
    return struct.pack("<%df" % len(vec), *vec)


def unpack(blob):
    return struct.unpack("<%df" % (len(blob) // 4), blob)


def table_of(key):
    table, sep, rest = key.partition(":")
    if not sep or not rest:
        raise ValueError("a key is table:id, such as docs:7")
    return table


def _quote(name):
    if not _NAME.match(name):
        raise ValueError("not a plain SQL name: %r" % name)
    return '"%s"' % name


def put(path, key, fields):
    """Inserts the record, or updates the fields given if it exists."""
    table = table_of(key)
    cols = sorted(fields)
    values = []
    for c in cols:
        v = fields[c]
        if c == "vec" and v is not None:
            v = pack(v)
        elif isinstance(v, (dict, list)):
            v = json.dumps(v)
        values.append(v)
    names = "".join(", " + _quote(c) for c in cols)
    marks = ", ?" * len(cols)
    if cols:
        update = "UPDATE SET " + ", ".join("%s = excluded.%s" % (_quote(c), _quote(c)) for c in cols)
    else:
        update = "NOTHING"
    sql = "INSERT INTO %s (key%s) VALUES (?%s) ON CONFLICT (key) DO %s" % (_quote(table), names, marks, update)
    _write(path, [(sql, [key] + values)])


def link(path, src, link_type, dst):
    _write(path, [("INSERT OR IGNORE INTO hc_links (src, type, dst) VALUES (?, ?, ?)", [src, link_type, dst])])


def delete(path, key):
    """Deletes the record. Its links go with it, by trigger."""
    _write(path, [("DELETE FROM %s WHERE key = ?" % _quote(table_of(key)), [key])])


def _write(path, statements):
    db = connect(path)
    try:
        db.execute("BEGIN IMMEDIATE")
        try:
            for sql, args in statements:
                db.execute(sql, args)
            db.execute("COMMIT")
        except BaseException:
            db.execute("ROLLBACK")
            raise
    finally:
        db.close()


def cosine(a, b):
    """Cosine distance: 0 for the same direction, 1 unrelated, 2 opposite."""
    dot = na = nb = 0.0
    for x, y in zip(a, b):
        dot += x * y
        na += x * x
        nb += y * y
    if na == 0 or nb == 0:
        return 1.0
    return min(2.0, max(0.0, 1 - dot / (math.sqrt(na) * math.sqrt(nb))))


def nearest(path, table, vec, k):
    """Compares every stored vector with vec and returns the k closest as
    (distance, key) pairs, closest first."""
    vec = unpack(pack(vec))  # float32 values, as HyperCrux compares them
    db = connect(path)
    try:
        rows = db.execute("SELECT key, vec FROM %s WHERE vec IS NOT NULL" % _quote(table)).fetchall()
    finally:
        db.close()
    hits = sorted((cosine(unpack(blob), vec), key) for key, blob in rows)
    return hits[:k]


def main(argv):
    if len(argv) < 3:
        print(__doc__.strip())
        return 2
    cmd, path = argv[1], argv[2]
    args = argv[3:]
    try:
        if cmd == "put" and len(args) == 2:
            put(path, args[0], json.loads(args[1]))
        elif cmd == "link" and len(args) == 3:
            link(path, *args)
        elif cmd == "delete" and len(args) == 1:
            delete(path, args[0])
        elif cmd == "nearest" and len(args) == 3:
            for d, key in nearest(path, args[0], json.loads(args[1]), int(args[2])):
                print("%.17g %s" % (d, key))
        elif cmd == "sql" and len(args) >= 1:
            db = connect(path)
            try:
                for row in db.execute(args[0], args[1:]):
                    print("\t".join("NULL" if v is None else str(v) for v in row))
            finally:
                db.close()
        else:
            print(__doc__.strip())
            return 2
    except (sqlite3.Error, ValueError) as e:
        print("error: %s" % e, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
