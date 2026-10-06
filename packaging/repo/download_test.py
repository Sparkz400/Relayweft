#!/usr/bin/env python3
"""Exercise build-pages.sh's real curl download path, without signing keys."""

import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest


class DownloadTest(unittest.TestCase):
    def check_download(self, token):
        packages = {
            f"relayweft-linux-{arch}.{kind}":
                f"package {arch} {kind}\n".encode() + b"\x00\xffbinary\n"
            for arch in ("amd64", "arm64")
            for kind in ("deb", "rpm", "apk")
        }
        assets = dict(packages)
        assets["checksums.txt"] = "".join(
            f"{hashlib.sha256(data).hexdigest()}  {name}\n"
            for name, data in packages.items()
        ).encode()
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                accepts = self.headers.get_all("Accept")
                requests.append((self.path, accepts))
                expected_auth = f"Bearer {token}" if token else None
                if self.headers.get("Authorization") != expected_auth:
                    self.send_error(401)
                    return
                if self.path.startswith("/repos/"):
                    body = json.dumps([{
                        "tag_name": "v0.4.0", "draft": False, "prerelease": False,
                        "assets": [
                            {"name": name, "url": f"{base}/assets/{name}"}
                            for name in (*assets, "relayweft-signing-key.asc")
                        ],
                    }]).encode()
                else:
                    name = self.path.removeprefix("/assets/")
                    # GitHub returns metadata for a JSON Accept header. A
                    # second octet-stream header must not leave that header in
                    # the request: the downloaded bytes would be JSON too.
                    body = (assets[name] if accepts == ["application/octet-stream"]
                            else json.dumps({"name": name, "size": len(assets[name])}).encode())
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        base = f"http://127.0.0.1:{server.server_port}"
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="rw-download-test-") as tmp:
                work = Path(tmp)
                script_dir = work / "packaging" / "repo"
                script_dir.mkdir(parents=True)
                script = script_dir / "build-pages.sh"
                source = Path(__file__).with_name("build-pages.sh").read_text()
                # Redirect only the API address; execute the actual downloader.
                script.write_text(source.replace("https://api.github.com", base))
                bin_dir = work / "bin"
                bin_dir.mkdir()
                docker = bin_dir / "docker"
                # Stop at the metadata boundary. The separate repository tests
                # exercise real signing and package-manager clients with Docker.
                docker.write_text(f"#!{sys.executable}\n" + '''
from pathlib import Path
import shutil
import sys
args = sys.argv[1:]
mounts = [args[i + 1] for i, arg in enumerate(args[:-1]) if arg == "-v"]
work = Path(next(m.removesuffix(":/work") for m in mounts if m.endswith(":/work")))
for kind in ("apt", "rpm", "apk"):
    (work / "out" / kind).mkdir(parents=True, exist_ok=True)
shutil.copytree(work / "in", work / "out" / "apt", dirs_exist_ok=True)
''')
                docker.chmod(0o755)
                env = dict(os.environ)
                env.update({
                    "PATH": f"{bin_dir}{os.pathsep}{env['PATH']}",
                    "PACKAGE_GPG_KEY": "test-only", "PACKAGE_APK_KEY": "test-only",
                    "GH_TOKEN": token, "GITHUB_TOKEN": "", "RW_REPO_SOURCE": "",
                    "RW_REPO_ARCHES": "amd64 arm64", "RW_REPO_KEEP": "3",
                    "RW_REPO_GITHUB": "test/repository", "RW_DOCKER_ARGS": "",
                })
                site = work / "site"
                result = subprocess.run(
                    ["sh", str(script), str(site)], env=env,
                    capture_output=True, text=True, timeout=30,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                for name, data in assets.items():
                    self.assertEqual((site / "apt" / "0.4.0" / name).read_bytes(), data)
                self.assertEqual(requests[0][1], ["application/vnd.github+json"])
                self.assertEqual(len(requests), len(assets) + 1)
                for path, accepts in requests[1:]:
                    self.assertEqual(accepts, ["application/octet-stream"], path)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_anonymous_downloads(self):
        self.check_download("")

    def test_authenticated_downloads(self):
        self.check_download("test-only-token")


if __name__ == "__main__":
    unittest.main()
