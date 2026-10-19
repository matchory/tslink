#!/bin/sh
# ready.sh <port> [log]: in a container, GET /ready of tslink's readiness endpoint on <port> and
# print "<epoch> <code> <seconds> <state> <services>"; code 000 when nothing answers. With a log
# ("-": standard output), append a line to it five times a second until killed.
exec python3 - "$@" <<'PY'
import json, sys, time, urllib.error, urllib.request

port = sys.argv[1]
if len(sys.argv) > 2 and sys.argv[2] != "-":
    log = open(sys.argv[2], "a", buffering=1)
else:
    log = sys.stdout
while True:
    t0 = time.time()
    try:
        r = urllib.request.urlopen("http://127.0.0.1:%s/ready" % port, timeout=5)
        code, body = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, body = e.code, e.read()
    except Exception:
        code, body = 0, b"{}"
    try:
        doc = json.loads(body)
    except ValueError:
        doc = {}
    services = json.dumps(doc.get("services", {}), separators=(",", ":"))
    print("%.1f %03d %.3f %s %s" % (t0, code, time.time() - t0, doc.get("state", "-"), services), file=log, flush=True)
    if len(sys.argv) < 3:
        break
    time.sleep(0.2)
PY
