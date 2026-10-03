#!/usr/bin/env python3
"""Create the GitHub release for a tag and upload dist/ as assets.

    scripts/publish_release.py 0.1.0

Needs GH_TOKEN in the environment (contents:write). Uses only the standard
library so it runs anywhere python3 does. Re-running is safe: an existing
release is reused and only missing or changed assets are uploaded.
"""

import json
import mimetypes
import os
import pathlib
import sys
import urllib.error
import urllib.request

REPO_SLUG = os.environ.get("REPO_SLUG", "cmyolo441-coder/Realgoagent")
API = "https://api.github.com"
DIST = pathlib.Path(__file__).resolve().parent.parent / "dist"

if len(sys.argv) != 2:
    sys.exit("usage: publish_release.py <version>")

version = sys.argv[1].lstrip("v")
tag = f"v{version}"

token = os.environ.get("GH_TOKEN")
if not token:
    sys.exit("GH_TOKEN is not set; it needs contents:write")

assets = sorted(p for p in DIST.iterdir() if p.suffix == ".gz" or p.name == "checksums.txt")
if not assets:
    sys.exit(f"no artifacts in {DIST}; run scripts/release.sh {version} first")


def request(method, url, *, data=None, headers=None):
    hdrs = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "nova-release-script",
    }
    hdrs.update(headers or {})
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req) as resp:
            body = resp.read()
            return resp.status, body
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()


def release():
    status, body = request("GET", f"{API}/repos/{REPO_SLUG}/releases/tags/{tag}")
    if status == 200:
        return json.loads(body)
    if status != 404:
        sys.exit(f"looking up {tag}: HTTP {status}\n{body.decode(errors='replace')}")
    return None


notes = f"""Automated release `{tag}`.

Install on Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/{REPO_SLUG}/main/install.sh | sh
```

Pin this version with `NOVA_VERSION={tag}`, or set `NOVA_INSTALL` to pick the
install directory.
"""

rel = release()
if rel:
    print(f"    release {tag} already exists, reusing it")
    upload_url = rel["upload_url"].split("{")[0]
    existing = {a["name"] for a in rel.get("assets", [])}
else:
    payload = json.dumps(
        {
            "tag_name": tag,
            "name": tag,
            "body": notes,
            "draft": False,
            "prerelease": False,
        }
    ).encode()
    status, body = request(
        "POST",
        f"{API}/repos/{REPO_SLUG}/releases",
        data=payload,
        headers={"Content-Type": "application/json"},
    )
    if status not in (200, 201):
        sys.exit(f"creating release: HTTP {status}\n{body.decode(errors='replace')}")
    rel = json.loads(body)
    print(f"    created release {tag}")
    upload_url = rel["upload_url"].split("{")[0]
    existing = set()

for asset in assets:
    name = asset.name
    # checksums.txt is cheap and must match the assets it describes.
    if name in existing and name != "checksums.txt":
        print(f"    {name}: already uploaded, skipping")
        continue

    ctype = "application/gzip"
    guessed = mimetypes.guess_type(name)[0]
    if guessed and not name.endswith(".txt"):
        ctype = guessed

    print(f"    {name}: uploading ({asset.stat().st_size // 1024} KiB)")
    status, body = request(
        "POST",
        f"{upload_url}?name={name}",
        data=asset.read_bytes(),
        headers={"Content-Type": ctype},
    )
    if status not in (201, 204):
        sys.exit(f"uploading {name}: HTTP {status}\n{body.decode(errors='replace')}")

print()
print(f"release: {rel['html_url']}")
print(f"digest : checksums.txt ({len(assets)} assets)")
