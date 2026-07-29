#!/usr/bin/env python3
"""Generate the SQLite fixtures the store tests read.

Real databases written by SQLite itself, so the reader is verified against the
actual file format rather than against something this repo also produced.

Run from this directory:  python3 generate.py
"""
import json
import os
import sqlite3

HERE = os.path.dirname(os.path.abspath(__file__))


def fresh(name):
    path = os.path.join(HERE, name)
    for suffix in ("", "-wal", "-shm", "-journal"):
        if os.path.exists(path + suffix):
            os.remove(path + suffix)
    conn = sqlite3.connect(path)
    # DELETE keeps everything in the main file, which is what kirogo can read.
    conn.execute("PRAGMA journal_mode=DELETE")
    return conn, path


def finish(conn):
    conn.commit()
    conn.execute("VACUUM")
    conn.close()


# ---------------------------------------------------------------- kiro-cli shape
# A database shaped like the real kiro-cli one: social token, device
# registration and a selected profile.
conn, _ = fresh("kirocli.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("CREATE TABLE state (key TEXT PRIMARY KEY, value BLOB)")
conn.execute(
    "INSERT INTO auth_kv VALUES (?, ?)",
    ("kirocli:social:token", json.dumps({
        "access_token": "cli-access-token",
        "refresh_token": "cli-refresh-token",
        "profile_arn": "arn:aws:codewhisperer:eu-central-1:111122223333:profile/CLIPROF12345",
        "region": "eu-central-1",
        "scopes": ["codewhisperer:completions", "codewhisperer:conversations"],
        "expires_at": "2030-01-02T03:04:05.123456789Z",
    })),
)
conn.execute(
    "INSERT INTO auth_kv VALUES (?, ?)",
    ("kirocli:odic:device-registration", json.dumps({
        "client_id": "cli-client-id",
        "client_secret": "cli-client-secret",
        "region": "eu-central-1",
    })),
)
conn.execute(
    "INSERT INTO state VALUES (?, ?)",
    ("api.codewhisperer.profile", json.dumps({
        "arn": "arn:aws:codewhisperer:eu-central-1:111122223333:profile/FROMSTATE123",
    })),
)
finish(conn)

# --------------------------------------------------- fallback key, no profile
# Only the legacy OIDC key, no profile in the token, so the state table supplies
# the ARN. Note the misspelled "odic", which is kiro-cli's own typo.
conn, _ = fresh("kirocli-legacy.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("CREATE TABLE state (key TEXT PRIMARY KEY, value BLOB)")
conn.execute(
    "INSERT INTO auth_kv VALUES (?, ?)",
    ("codewhisperer:odic:token", json.dumps({
        "access_token": "legacy-access",
        "refresh_token": "legacy-refresh",
        "expires_at": "2030-06-07T08:09:10Z",
    })),
)
conn.execute(
    "INSERT INTO auth_kv VALUES (?, ?)",
    ("codewhisperer:odic:device-registration", json.dumps({
        "client_id": "legacy-client-id",
        "client_secret": "legacy-client-secret",
        "region": "us-west-2",
    })),
)
conn.execute(
    "INSERT INTO state VALUES (?, ?)",
    ("api.codewhisperer.profile", json.dumps({
        "arn": "arn:aws:codewhisperer:us-west-2:999988887777:profile/LEGACY123456",
    })),
)
finish(conn)

# ------------------------------------------------------------- priority order
# Several token keys at once. The social key must win.
conn, _ = fresh("kirocli-multiple.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
for key, marker in (
    ("codewhisperer:odic:token", "third"),
    ("kirocli:odic:token", "second"),
    ("kirocli:social:token", "first"),
):
    conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
                 (key, json.dumps({"access_token": marker + "-access",
                                   "refresh_token": marker + "-refresh"})))
finish(conn)

# ------------------------------------------------------------- overflow pages
# Values far larger than one page, which forces overflow chains, plus a
# multi-page table so the B-tree gains interior nodes.
conn, _ = fresh("overflow.sqlite3")
conn.execute("PRAGMA page_size=512")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("small", "tiny"))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("one-page", "A" * 400))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("spills", "B" * 5000))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("spills-far", "C" * 200000))
conn.execute(
    "INSERT INTO auth_kv VALUES (?, ?)",
    ("kirocli:social:token", json.dumps({
        "access_token": "X" * 4000,
        "refresh_token": "Y" * 4000,
        "expires_at": "2030-01-01T00:00:00Z",
        "region": "us-east-1",
    })),
)
finish(conn)

# ------------------------------------------------------- many rows, deep tree
# Enough rows at a small page size to require interior B-tree pages.
conn, _ = fresh("manyrows.sqlite3")
conn.execute("PRAGMA page_size=512")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
for i in range(2000):
    conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
                 (f"key-{i:05d}", f"value-{i:05d}-" + "z" * 80))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:social:token",
              json.dumps({"access_token": "deep-access", "refresh_token": "deep-refresh"})))
finish(conn)

# ---------------------------------------------------------- every value type
conn, _ = fresh("types.sqlite3")
conn.execute("CREATE TABLE vals (key TEXT PRIMARY KEY, value BLOB)")
rows = [
    ("null", None),
    ("int-zero", 0),
    ("int-one", 1),
    ("int-small", 42),
    ("int-negative", -42),
    ("int-16", 30000),
    ("int-24", 8000000),
    ("int-32", 2000000000),
    ("int-48", 100000000000),
    ("int-64", 9000000000000000000),
    ("int-neg-64", -9000000000000000000),
    ("float", 3.5),
    ("float-negative", -0.125),
    ("text", "hello world"),
    ("text-unicode", "\u00fc\u00f1\u00ee\u00e7\u00f8d\u00e9 \u4f60\u597d"),
    ("text-empty", ""),
    ("blob", sqlite3.Binary(bytes([0, 1, 2, 255, 254]))),
    ("blob-empty", sqlite3.Binary(b"")),
]
for key, value in rows:
    conn.execute("INSERT INTO vals VALUES (?, ?)", (key, value))
finish(conn)

# -------------------------------------------------------- unusual column order
# The value column comes first, so a reader that assumes positions gets it wrong.
conn, _ = fresh("column-order.sqlite3")
conn.execute("""CREATE TABLE auth_kv (
    value BLOB,
    unrelated INTEGER DEFAULT 0,
    "key" TEXT,
    PRIMARY KEY ("key")
)""")
conn.execute('INSERT INTO auth_kv (value, "key") VALUES (?, ?)',
             (json.dumps({"access_token": "reordered-access",
                          "refresh_token": "reordered-refresh"}),
              "kirocli:social:token"))
finish(conn)

# ------------------------------------------------------------ larger page size
conn, _ = fresh("page16k.sqlite3")
conn.execute("PRAGMA page_size=16384")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:social:token",
              json.dumps({"access_token": "big-page-access", "refresh_token": "big-page-refresh"})))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("huge", "Q" * 40000))
finish(conn)

# ------------------------------------------------------------------ no tables
conn, _ = fresh("empty.sqlite3")
finish(conn)

# ------------------------------------------------- table present but no rows
conn, _ = fresh("norows.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("CREATE TABLE state (key TEXT PRIMARY KEY, value BLOB)")
finish(conn)

# ------------------------------------------------------------------- WAL mode
# Removed and rebuilt each run, so regenerating is idempotent.
for suffix in ("", "-wal", "-shm", "-journal"):
    path = os.path.join(HERE, "walmode.sqlite3" + suffix)
    if os.path.exists(path):
        os.remove(path)
conn = sqlite3.connect(os.path.join(HERE, "walmode.sqlite3"))
conn.execute("PRAGMA journal_mode=WAL")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:social:token", json.dumps({"access_token": "wal-access"})))
conn.commit()
conn.close()
# Remove the sidecars so the header check, not the sidecar check, is exercised.
for suffix in ("-wal", "-shm"):
    path = os.path.join(HERE, "walmode.sqlite3" + suffix)
    if os.path.exists(path):
        os.remove(path)

# ----------------------------------------------------------- malformed inputs
with open(os.path.join(HERE, "notsqlite.bin"), "wb") as fh:
    fh.write(b"this is definitely not a SQLite database, not even close\n" * 4)

with open(os.path.join(HERE, "truncated.sqlite3"), "wb") as fh:
    fh.write(b"SQLite format 3\x00")  # header magic only

with open(os.path.join(HERE, "zerolength.sqlite3"), "wb") as fh:
    pass

# A valid header with a nonsensical page size.
with open(os.path.join(HERE, "kirocli.sqlite3"), "rb") as fh:
    good = bytearray(fh.read())
bad = bytearray(good)
bad[16:18] = (777).to_bytes(2, "big")  # not a power of two
with open(os.path.join(HERE, "badpagesize.sqlite3"), "wb") as fh:
    fh.write(bad)

print("fixtures written to", HERE)
for name in sorted(os.listdir(HERE)):
    if name != "generate.py":
        size = os.path.getsize(os.path.join(HERE, name))
        print(f"  {name:<30} {size:>8} bytes")

# ------------------------------------------------ unreadable expiry timestamp
# An expiry that cannot be parsed must not fail the load: unknown simply means
# "refresh now".
conn, _ = fresh("bad-expiry.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:social:token", json.dumps({
                 "access_token": "a", "refresh_token": "r", "expires_at": "whenever"})))
finish(conn)

# ------------------------------------------- malformed JSON in a token row
# The highest-priority key holds junk, so the next key must be tried.
conn, _ = fresh("bad-json.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)", ("kirocli:social:token", "{not json at all"))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:odic:token", json.dumps({
                 "access_token": "second-choice", "refresh_token": "r"})))
finish(conn)

# --------------------------------------------- valid JSON carrying no tokens
conn, _ = fresh("no-tokens.sqlite3")
conn.execute("CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)")
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:social:token", json.dumps({"region": "us-east-1"})))
conn.execute("INSERT INTO auth_kv VALUES (?, ?)",
             ("kirocli:odic:token", json.dumps({
                 "access_token": "real", "refresh_token": "r"})))
finish(conn)

print("supplementary fixtures written")
