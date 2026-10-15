#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Checks the links and runs the examples in the Beta's docs (task R3), and
# in the site's posts about the Beta as well when it's given the path of a
# clone of the site, github.com/photozeitgeist/hypercrux.com. It needs Go,
# and Hugo for the site.
#
#   python3 scripts/check-docs.py                   # this repository's docs
#   python3 scripts/check-docs.py ../hypercrux.com  # and the site's posts too
#
# The site's repository is private, so where the site can't be checked out,
# as in CI, it runs with no argument. Whoever publishes the posts runs it with
# the site's path before they go up.
#
# Links. It checks three kinds, and lists the rest, which it leaves alone,
# since a check here can't reach other websites:
#   - links to files and folders of this repository, relative to the doc,
#     and to their headings' anchors, worked out as GitHub works them out;
#   - links to the site's own pages and images, https://hypercrux.com/...,
#     looked up in the site as `hugo --quiet -d DIR` builds it, in a folder
#     outside both repositories, with the anchors in the page. Without the
#     site, they're left alone and listed with the others;
#   - links to GitHub's pages of this repository, .../blob/main/PATH and
#     .../tree/main/PATH, by the file or folder they name, with the anchor.
#
# Examples. A fenced block marked by a comment on the line before it runs:
#   <!-- run -->      a sh block runs with `sh -e`, the Beta's command first
#                     on PATH as hypercrux, and a go block, a whole program,
#                     is built against the Beta's package in this repository
#                     and run;
#   <!-- run 0.x -->  a sh block runs the same way with 0.x's command, for
#                     the step that exports from 0.x;
#   <!-- output -->   the block holds what the run before it prints, which
#                     has to match it exactly.
# The examples of one doc run in order in one new folder, so a block can use
# the files the blocks before it made. Both commands are built here and now,
# the Beta's with CGO_ENABLED=0 and 0.x's with cgo, which needs a C compiler.
# Every other code block is left alone, and counted.
import difflib
import html
import html.parser
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unicodedata
import urllib.parse

ROOT = os.path.realpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
MODULE = "github.com/hypercrux/hypercrux"
SITE_HOSTS = ("hypercrux.com", "www.hypercrux.com")

# The docs R3 wrote or touched in this repository, and its posts in the site.
REPO_DOCS = [
    "README.md",
    "beta/README.md",
    "beta/FORMAT.md",
    "beta/MOVING.md",
    "beta/SQL.md",
    "beta/tasks/R3.md",
]
SITE_DOCS = [
    "content/posts/how-the-hypercrux-beta-was-tested.md",
    "content/posts/moving-from-hypercrux-0x-to-the-beta.md",
]

MARKER = re.compile(r"^<!--\s*(run|run 0\.x|output)\s*-->\s*$")
FENCE = re.compile(r"^\s*(`{3,}|~{3,})(.*)$")  # a list item's fence is indented too


class Doc:
    def __init__(self, path, name, in_site):
        self.path = path
        self.name = name
        self.in_site = in_site
        self.text = open(path, encoding="utf-8").read()
        self.lines = self.text.split("\n")


class Block:
    def __init__(self, doc, line, lang, body, mark):
        self.doc = doc
        self.line = line
        self.lang = lang
        self.body = body
        self.mark = mark


problems = []


def problem(where, what):
    problems.append(f"{where}: {what}")


def parse(doc):
    """Splits a doc into its code blocks and its prose, as (line, text)
    paragraphs, with front matter and code blocks taken out."""
    blocks, paras = [], []
    lines = doc.lines
    i = 0
    if doc.in_site and lines and lines[0] == "---":
        if "---" not in lines[1:]:
            problem(f"{doc.name}:1", "front matter that never ends")
            return blocks, paras
        end = lines.index("---", 1)
        for j in range(1, end):
            m = re.match(r"^featured_image:\s*\"?([^\"]+)\"?\s*$", lines[j])
            if m:
                paras.append((j + 1, "", [m.group(1)]))
        i = end + 1
    para, start = [], 0
    mark, mark_line = None, -1
    while i < len(lines):
        line = lines[i]
        m = FENCE.match(line)
        if m:
            if para:
                paras.append((start, "\n".join(para), None))
                para = []
            opened = i
            fence = m.group(1)
            lang = (m.group(2).strip().split() or [""])[0]
            body = []
            i += 1
            while i < len(lines):
                c = re.match(r"^\s*(`{3,}|~{3,})\s*$", lines[i])
                if c and c.group(1)[0] == fence[0] and len(c.group(1)) >= len(fence):
                    break
                body.append(lines[i])
                i += 1
            if i == len(lines):
                problem(f"{doc.name}:{opened + 1}", "a code block that never ends")
            use = mark if mark_line == opened - 1 else None
            blocks.append(Block(doc, opened + 1, lang, "\n".join(body) + "\n", use))
            mark = None
            i += 1
            continue
        mm = MARKER.match(line)
        if mm:
            mark, mark_line = mm.group(1), i
            nxt = lines[i + 1] if i + 1 < len(lines) else ""
            if not FENCE.match(nxt):
                problem(f"{doc.name}:{i + 1}", f"<!-- {mark} --> isn't followed by a code block on the next line")
        if line.strip() == "":
            if para:
                paras.append((start, "\n".join(para), None))
                para = []
        else:
            if not para:
                start = i + 1
            para.append(line)
        i += 1
    if para:
        paras.append((start, "\n".join(para), None))
    return blocks, paras


INLINE = re.compile(r"(!?)\[((?:[^\[\]]|\[[^\[\]]*\])*)\]\(\s*<?([^\s()<>]*)>?(?:\s+\"[^\"]*\")?\s*\)")
REFDEF = re.compile(r"^ {0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+\"[^\"]*\")?\s*$", re.M)
AUTO = re.compile(r"<(https?://[^>\s]+)>")
BARE = re.compile(r"https?://[^\s<>()\[\]`\"']+")
HTMLREF = re.compile(r"(?:href|src)=\"([^\"]+)\"")


def links(doc, paras):
    """Every link in the doc's prose, as (line, url)."""
    found = []
    for line, text, given in paras:
        if given:
            found += [(line, u) for u in given]
            continue
        # Inline code spans aren't links, and they can run over a line.
        text = re.sub(r"(`+)(.+?)\1", lambda m: re.sub(r"[^\n]", " ", m.group(0)), text, flags=re.S)

        def at(pos):
            return line + text.count("\n", 0, pos)

        def take(pattern, group, s):
            out = []

            def cut(m):
                out.append((at(m.start()), m.group(group)))
                return re.sub(r"[^\n]", " ", m.group(0))

            return pattern.sub(cut, s), out

        text, a = take(INLINE, 3, text)
        text, b = take(REFDEF, 1, text)
        text, c = take(HTMLREF, 1, text)
        text, d = take(AUTO, 1, text)
        e = []
        for m in BARE.finditer(text):
            url = m.group(0).rstrip(".,;:!?")
            e.append((at(m.start()), url))
        found += a + b + c + d + e
    return found


def slug(text):
    """A heading's anchor, as GitHub makes it."""
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.replace("`", "").replace("*", "").replace("\\", "")
    out = []
    for ch in text.strip().lower():
        if ch == " ":
            out.append("-")
        elif ch in "-_" or unicodedata.category(ch)[0] in "LMN" or unicodedata.category(ch) == "Pc":
            out.append(ch)
    return "".join(out)


anchors_of = {}


def md_anchors(path):
    if path not in anchors_of:
        seen, found = {}, set()
        in_fence = None
        for line in open(path, encoding="utf-8").read().split("\n"):
            m = FENCE.match(line)
            if m:
                if in_fence is None:
                    in_fence = m.group(1)
                elif m.group(1)[0] == in_fence[0] and len(m.group(1)) >= len(in_fence) and not m.group(2).strip():
                    in_fence = None
                continue
            if in_fence:
                continue
            h = re.match(r"^ {0,3}#{1,6}\s+(.*?)\s*#*\s*$", line)
            if h:
                s = slug(h.group(1))
                n = seen.get(s, 0)
                seen[s] = n + 1
                found.add(s if n == 0 else f"{s}-{n}")
            for a in re.finditer(r"<a\s+(?:id|name)=\"([^\"]+)\"", line):
                found.add(a.group(1))
        anchors_of[path] = found
    return anchors_of[path]


class IDs(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = set()

    def handle_starttag(self, tag, attrs):
        for k, v in attrs:
            if k in ("id", "name") and v:
                self.ids.add(v)


def html_anchors(path):
    if path not in anchors_of:
        p = IDs()
        p.feed(open(path, encoding="utf-8", errors="replace").read())
        anchors_of[path] = p.ids
    return anchors_of[path]


def check_repo_path(where, rel, anchor, want=None):
    """rel is a path in this repository, from its root."""
    full = os.path.normpath(os.path.join(ROOT, rel))
    if not (full == ROOT or full.startswith(ROOT + os.sep)):
        problem(where, f"{rel} is outside the repository")
        return
    if not os.path.exists(full):
        problem(where, f"{rel} doesn't exist in the repository")
        return
    if want == "file" and not os.path.isfile(full):
        problem(where, f"{rel} isn't a file, as a blob link needs")
    if want == "dir" and not os.path.isdir(full):
        problem(where, f"{rel} isn't a folder, as a tree link needs")
    if anchor:
        target = full
        if os.path.isdir(full):
            target = os.path.join(full, "README.md")
        if not target.endswith(".md") or not os.path.isfile(target):
            problem(where, f"#{anchor} points into {rel}, which has no headings to check")
        elif anchor != "readme" or not os.path.isdir(full):
            if anchor not in md_anchors(target):
                problem(where, f"{rel} has no heading with the anchor #{anchor}")


def check_site_path(where, path, anchor, site):
    path = urllib.parse.unquote(path or "/")
    rel = path.lstrip("/")
    full = os.path.join(site, rel)
    page = None
    if path.endswith("/"):
        page = os.path.join(full, "index.html")
    elif os.path.isfile(full):
        page = full
    elif os.path.isfile(os.path.join(full, "index.html")):
        page = os.path.join(full, "index.html")
    if page is None or not os.path.isfile(page):
        problem(where, f"the site has no page at {path}")
        return
    if anchor and anchor not in html_anchors(page):
        problem(where, f"the site's page at {path} has no anchor #{anchor}")


def check_links(doc, paras, site, tally, left):
    for line, url in links(doc, paras):
        where = f"{doc.name}:{line}"
        if not url:
            problem(where, "a link with no destination")
            continue
        parts = urllib.parse.urlsplit(url)
        anchor = urllib.parse.unquote(parts.fragment) if parts.fragment else ""
        if parts.scheme in ("http", "https"):
            host = parts.netloc.lower()
            if host in SITE_HOSTS:
                if parts.scheme != "https":
                    problem(where, f"{url} should use https")
                if site is None:
                    left.append((where, url))  # the site wasn't given
                    tally["site_left"] += 1
                    continue
                check_site_path(where, parts.path, anchor, site)
                tally["site"] += 1
                continue
            if host == "github.com":
                m = re.match(r"^/hypercrux/hypercrux(?:/(blob|tree)/main(/.*)?)?/?$", parts.path)
                if m:
                    kind, rel = m.group(1), (m.group(2) or "").strip("/")
                    want = {"blob": "file", "tree": "dir"}.get(kind)
                    check_repo_path(where, rel or ".", anchor or None, want)
                    tally["github"] += 1
                    continue
            left.append((where, url))
            continue
        if parts.scheme:
            left.append((where, url))
            continue
        if doc.in_site:
            if parts.path and not parts.path.startswith("/"):
                problem(where, f"{url}: a post's links to the site start with https://hypercrux.com/")
                continue
            path = parts.path or page_of(doc, site)
            if path is None:
                problem(where, f"{url}: the site as built has no page for this post")
                continue
            check_site_path(where, path, anchor, site)
            tally["site"] += 1
            continue
        # GitHub reads a path that starts with a slash from the repository's
        # root, and any other from the doc's folder.
        path = urllib.parse.unquote(parts.path)
        base = "" if path.startswith("/") else os.path.dirname(doc.name)
        rel = os.path.normpath(os.path.join(base, path.lstrip("/"))) if path else doc.name
        check_repo_path(where, rel, anchor or None)
        tally["repo"] += 1


titles = {}


def page_of(doc, site):
    """The path of a post's page in the site as built, found by its title."""
    if not titles:
        for d, _, files in os.walk(site):
            if "index.html" in files:
                page = open(os.path.join(d, "index.html"), encoding="utf-8", errors="replace").read()
                m = re.search(r"<title>(.*?)</title>", page, re.S)
                if m:
                    path = "/" + os.path.relpath(d, site).replace(os.sep, "/") + "/"
                    titles.setdefault(html.unescape(m.group(1)).strip(), path.replace("/./", "/"))
    m = re.search(r"^title:\s*\"(.*)\"\s*$", doc.text, re.M)
    if not m:
        return None
    return titles.get(m.group(1) + " | HyperCrux.com")


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


def build(tmp, need_zerox):
    env = dict(os.environ, CGO_ENABLED="0")
    beta = os.path.join(tmp, "beta")
    os.makedirs(beta)
    r = run(["go", "build", "-o", os.path.join(beta, "hypercrux"), "./beta/cmd/hypercrux"], cwd=ROOT, env=env)
    if r.returncode:
        sys.exit("building the Beta's command failed:\n" + r.stdout + r.stderr)
    zerox = None
    if need_zerox:
        zerox = os.path.join(tmp, "zerox")
        os.makedirs(zerox)
        r = run(["go", "build", "-o", os.path.join(zerox, "hypercrux"), "./cmd/hypercrux"], cwd=ROOT,
                env=dict(os.environ, CGO_ENABLED="1"))
        if r.returncode:
            sys.exit("building 0.x's command failed, which needs cgo and a C compiler:\n" + r.stdout + r.stderr)
    os.makedirs(os.path.join(tmp, "bin"))
    mod = os.path.join(tmp, "gomod")
    os.makedirs(mod)
    with open(os.path.join(mod, "go.mod"), "w") as f:
        f.write(f"module example\n\ngo 1.24\n\nrequire {MODULE} v0.0.0\n\nreplace {MODULE} => {ROOT}\n")
    shutil.copy(os.path.join(ROOT, "go.sum"), mod)
    return beta, zerox, mod


def run_examples(docs_blocks, tmp, counts):
    marked = [b for blocks in docs_blocks for b in blocks if b.mark]
    if not marked:
        return
    beta, zerox, mod = build(tmp, any(b.mark == "run 0.x" for b in marked))
    goenv = dict(os.environ, CGO_ENABLED="0", GOFLAGS="-mod=mod", GOWORK="off")
    n = 0
    for blocks in docs_blocks:
        if not any(b.mark for b in blocks):
            continue
        doc = blocks[0].doc
        work = os.path.join(tmp, "work", re.sub(r"[^A-Za-z0-9.]+", "-", doc.name))
        os.makedirs(work)
        last = None
        for b in blocks:
            where = f"{doc.name}:{b.line}"
            if not b.mark:
                counts["left"] += 1
                last = None
                continue
            if b.mark == "output":
                if last is None:
                    problem(where, "<!-- output --> doesn't follow a block that ran")
                    continue
                got, want = last.rstrip("\n"), b.body.rstrip("\n")
                if got != want:
                    diff = "\n".join(difflib.unified_diff(want.split("\n"), got.split("\n"),
                                                          "the doc", "the run", lineterm=""))
                    problem(where, "the output differs from the run's:\n" + diff)
                counts["outputs"] += 1
                last = None
                continue
            n += 1
            if b.lang == "go":
                if not re.search(r"^package main\b", b.body, re.M):
                    problem(where, "a Go example to run is a whole program, package main")
                    continue
                pkg = os.path.join(mod, f"ex{n}")
                os.makedirs(pkg)
                with open(os.path.join(pkg, "main.go"), "w") as f:
                    f.write(b.body)
                exe = os.path.join(tmp, "bin", f"ex{n}")
                r = run(["go", "build", "-o", exe, f"./ex{n}"], cwd=mod, env=goenv)
                if r.returncode:
                    problem(where, "the Go example doesn't build:\n" + r.stdout + r.stderr)
                    last = None
                    continue
                cmd = [exe]
                counts["go"] += 1
            elif b.lang in ("sh", "shell", "bash"):
                script = os.path.join(tmp, f"ex{n}.sh")
                with open(script, "w") as f:
                    f.write(b.body)
                cmd = ["sh", "-e", script]
                counts["sh0x" if b.mark == "run 0.x" else "sh"] += 1
            else:
                problem(where, f"<!-- {b.mark} --> on a {b.lang or 'plain'} block; only sh and go blocks run")
                continue
            bindir = zerox if b.mark == "run 0.x" else beta
            env = dict(os.environ, PATH=bindir + os.pathsep + os.environ.get("PATH", ""), LC_ALL="C")
            try:
                r = run(cmd, cwd=work, env=env, timeout=300)
            except subprocess.TimeoutExpired:
                problem(where, "the example ran for more than 5 minutes")
                last = None
                continue
            if r.returncode:
                problem(where, f"the example failed with exit status {r.returncode}:\n" + r.stdout + r.stderr)
                last = None
                continue
            last = r.stdout


def build_site(site, tmp):
    out = os.path.join(tmp, "site")
    env = dict(os.environ, HUGO_RESOURCEDIR=os.path.join(tmp, "hugo-resources"))
    r = run(["hugo", "--quiet", "--noBuildLock", "--source", site, "--cacheDir", os.path.join(tmp, "hugo-cache"),
             "-d", out], env=env)
    if r.returncode:
        sys.exit("hugo couldn't build the site:\n" + r.stdout + r.stderr)
    return out


def main():
    if len(sys.argv) > 2 or sys.argv[1:] in (["-h"], ["--help"]):
        sys.exit("usage: python3 scripts/check-docs.py [SITE]\n"
                 "SITE is the path of a clone of the site's repository. With it, the site's\n"
                 "posts about the Beta are checked too. Without it, the docs' links to the\n"
                 "site's pages are left alone, and listed.")
    site_src = None
    if len(sys.argv) == 2:
        site_src = os.path.realpath(sys.argv[1])
        if not os.path.isfile(os.path.join(site_src, "config.toml")):
            sys.exit(f"{site_src} isn't the site: it has no config.toml")
    for tool in ("go", "hugo") if site_src else ("go",):
        if not shutil.which(tool):
            sys.exit(f"{tool} isn't on PATH, and the check needs it")
    tmp = tempfile.mkdtemp(prefix="hypercrux-docs-")
    for repo in (ROOT, site_src) if site_src else (ROOT,):
        if os.path.commonpath([repo, tmp]) == repo:
            sys.exit(f"the temporary folder {tmp} is inside {repo}; set TMPDIR to a folder outside it")
    try:
        site = build_site(site_src, tmp) if site_src else None
        docs = []
        sources = [(ROOT, REPO_DOCS, False)]
        if site_src:
            sources.append((site_src, SITE_DOCS, True))
        for base, names, in_site in sources:
            for d in names:
                name = "site/" + d if in_site else d
                if os.path.isfile(os.path.join(base, d)):
                    docs.append(Doc(os.path.join(base, d), name, in_site))
                else:
                    problem(name, "the doc isn't there")
        tally = {"repo": 0, "site": 0, "github": 0, "site_left": 0}
        left = []
        counts = {"sh": 0, "sh0x": 0, "go": 0, "outputs": 0, "left": 0}
        all_blocks = []
        posts = []
        for doc in docs:
            blocks, paras = parse(doc)
            check_links(doc, paras, site, tally, left)
            all_blocks.append(blocks)
            if doc.in_site:
                path = page_of(doc, site)
                if path is None:
                    problem(doc.name, "the site as built has no page with this post's title")
                posts.append((doc.name, path))
        run_examples(all_blocks, tmp, counts)
        counts["left"] += sum(len(bs) for bs in all_blocks if not any(b.mark for b in bs))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    in_site = sum(1 for d in docs if d.in_site)
    if site_src:
        print(f"Docs: {len(docs)}, {len(docs) - in_site} in this repository and {in_site} posts in the site.")
    else:
        print(f"Docs: {len(docs)}, all in this repository. The site wasn't given, so its posts weren't read.")
    for name, path in posts:
        print(f"  {name} is at https://hypercrux.com{path or ' (not found)'}")
    print(f"Links checked: {tally['repo'] + tally['site'] + tally['github']}")
    print(f"  {tally['repo']} to files, folders and anchors in this repository")
    if site_src:
        print(f"  {tally['site']} to the site's own pages and images, in the site as Hugo built it")
    print(f"  {tally['github']} to GitHub's pages of this repository, by the files they name")
    print(f"Links left alone: {len(left)}")
    if tally["site_left"]:
        print(f"  {tally['site_left']} to the site's own pages, which weren't looked up, since the site wasn't given")
    print(f"  {len(left) - tally['site_left']} to other websites, which a check here can't reach")
    for where, url in left:
        print(f"  {url}  ({where})")
    ran = counts["sh"] + counts["sh0x"] + counts["go"]
    print(f"Examples run: {ran}, {counts['sh']} in sh on the Beta's command, {counts['sh0x']} in sh on 0.x's, "
          f"and {counts['go']} in Go on the Beta's package, with {counts['outputs']} outputs compared")
    print(f"Code blocks left alone, not marked to run: {counts['left']}")
    if problems:
        print(f"\n{len(problems)} problems:")
        for p in problems:
            print("  " + p.replace("\n", "\n    "))
        sys.exit(1)
    print("The docs' links and examples are sound.")


if __name__ == "__main__":
    main()
