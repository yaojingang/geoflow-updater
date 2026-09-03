#!/usr/bin/env python3
"""Serve a local candidate TUF repository over TLS on a disposable runner."""

import argparse
import functools
import http.server
import os
import ssl


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--certificate", required=True)
    parser.add_argument("--private-key", required=True)
    parser.add_argument("--port", required=True, type=int)
    arguments = parser.parse_args()

    directory = os.path.realpath(arguments.directory)
    if not os.path.isdir(directory):
        raise SystemExit("repository directory is unavailable")
    if arguments.port < 1024 or arguments.port > 65535:
        raise SystemExit("port must be unprivileged")

    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=directory)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", arguments.port), handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(arguments.certificate, arguments.private_key)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
