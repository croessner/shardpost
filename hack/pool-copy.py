#!/usr/bin/env python3
"""Copy every object of a RADOS pool into another pool and verify the copy.

Used once for the pool rename imap-* -> shardpost-* (2026-09-18) and kept as a
tool: the target pools are created by Rook, the servers are stopped, then this
script copies all namespaces with data, xattrs and omap (omap only where the
source has it; EC pools have none) and compares source and target object by
object. Object versions are not copied: the servers use them only for live
compare-and-swap, never persist them.

Run inside the Rook toolbox (python3-rados, admin keyring):

  pool-copy.py copy   <src-pool> <dst-pool>   # copy, then verify
  pool-copy.py verify <src-pool> <dst-pool>   # verify only

Exit code 0 only if the target holds exactly the source's objects with equal
data, xattrs and omap; the script refuses a non-empty target for "copy".
"""
import hashlib
import sys
import threading
from concurrent.futures import ThreadPoolExecutor

import rados

OMAP_PAGE = 1000
READ_CHUNK = 4 << 20
# Rook initialises every new pool for RBD; these objects may already exist.
POOL_INIT = {("", "rbd_info"), ("", "rbd_trash")}
WORKERS = 16

_local = threading.local()


def ioctx_for(cluster, pool):
    """One IoCtx per thread and pool (an IoCtx carries the namespace)."""
    key = "io_" + pool
    io = getattr(_local, key, None)
    if io is None:
        io = cluster.open_ioctx(pool)
        setattr(_local, key, io)
    return io


def parallel(fn, items):
    """Run fn over items with WORKERS threads; librados releases the GIL."""
    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        return list(ex.map(fn, items))


def objects(ioctx):
    """All (namespace, name) pairs of a pool, over all namespaces."""
    ioctx.set_namespace(rados.LIBRADOS_ALL_NSPACES)
    out = [(o.nspace, o.key) for o in ioctx.list_objects()]
    return sorted(out)


def read_all(ioctx, name):
    size, _ = ioctx.stat(name)
    buf = bytearray()
    off = 0
    while off < size:
        chunk = ioctx.read(name, min(READ_CHUNK, size - off), off)
        if not chunk:
            break
        buf += chunk
        off += len(chunk)
    return bytes(buf)


def xattrs(ioctx, name):
    return {k: bytes(v) for k, v in ioctx.get_xattrs(name)}


def omap(ioctx, name):
    vals = {}
    after = ""
    while True:
        with rados.ReadOpCtx() as op:
            it, ret = ioctx.get_omap_vals(op, after, "", OMAP_PAGE)
            ioctx.operate_read_op(op, name)
            page = [(k, bytes(v)) for k, v in it]
        vals.update(page)
        if len(page) < OMAP_PAGE:
            return vals
        after = page[-1][0]


def digest(ioctx, ns, name, with_omap):
    ioctx.set_namespace(ns)
    h = hashlib.sha256()
    h.update(read_all(ioctx, name))
    for k, v in sorted(xattrs(ioctx, name).items()):
        h.update(b"x" + k.encode() + b"\0" + v)
    if with_omap:
        for k, v in sorted(omap(ioctx, name).items()):
            h.update(b"o" + k.encode() + b"\0" + v)
    return h.hexdigest()


def supports_omap(cluster, pool):
    # EC pools cannot hold omap
    out = cluster.mon_command('{"prefix": "osd pool get", "pool": "%s", "var": "erasure_code_profile", "format": "json"}' % pool, b"")
    return out[0] != 0  # ENOENT-like error: replicated pool


def copy(cluster, src, dst):
    s = cluster.open_ioctx(src)
    d = cluster.open_ioctx(dst)
    if set(objects(d)) - POOL_INIT:
        sys.exit("target pool %s is not empty" % dst)
    with_omap = supports_omap(cluster, src)
    objs = objects(s)

    def one(obj):
        ns, name = obj
        si, di = ioctx_for(cluster, src), ioctx_for(cluster, dst)
        si.set_namespace(ns)
        di.set_namespace(ns)
        di.write_full(name, read_all(si, name))
        for k, v in xattrs(si, name).items():
            di.set_xattr(name, k, v)
        if with_omap:
            vals = omap(si, name)
            keys = list(vals)
            for j in range(0, len(keys), OMAP_PAGE):
                part = keys[j:j + OMAP_PAGE]
                with rados.WriteOpCtx() as op:
                    di.set_omap(op, tuple(part), tuple(vals[k] for k in part))
                    di.operate_write_op(op, name)

    parallel(one, objs)
    print("copied %d objects from %s to %s" % (len(objs), src, dst), flush=True)


def verify(cluster, src, dst):
    s = cluster.open_ioctx(src)
    d = cluster.open_ioctx(dst)
    so, do = objects(s), objects(d)
    if so != do:
        missing = set(so) - set(do)
        extra = set(do) - set(so)
        print("object sets differ: %d missing, %d extra" % (len(missing), len(extra)))
        return False
    with_omap = supports_omap(cluster, src)

    def same(obj):
        ns, name = obj
        a = digest(ioctx_for(cluster, src), ns, name, with_omap)
        b = digest(ioctx_for(cluster, dst), ns, name, with_omap)
        if a != b:
            print("differs: ns=%r obj=%r" % (ns, name), flush=True)
        return a == b

    bad = sum(1 for ok in parallel(same, so) if not ok)
    namespaces = len({ns for ns, _ in so})
    print("verified %d objects in %d namespaces (omap=%s): %d differ" % (len(so), namespaces, with_omap, bad))
    return bad == 0


def main():
    if len(sys.argv) != 4 or sys.argv[1] not in ("copy", "verify"):
        sys.exit(__doc__)
    mode, src, dst = sys.argv[1:]
    cluster = rados.Rados(conffile="/etc/ceph/ceph.conf")
    cluster.connect()
    try:
        if mode == "copy":
            copy(cluster, src, dst)
        ok = verify(cluster, src, dst)
    finally:
        cluster.shutdown()
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
