"""CLI entry: toolforge serve."""

from __future__ import annotations

import argparse
import os
import sys


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(prog="toolforge", description="ToolForge LLM tool-calling middleware")
    sub = parser.add_subparsers(dest="command")

    serve = sub.add_parser("serve", help="Start the HTTP proxy")
    serve.add_argument("-c", "--config", default=None, help="Path to config.yaml")
    serve.add_argument("--host", default=None)
    serve.add_argument("--port", type=int, default=None)
    serve.add_argument("--reload", action="store_true")

    args = parser.parse_args(argv)
    if args.command != "serve":
        parser.print_help()
        sys.exit(0 if args.command is None else 1)

    if args.config:
        os.environ["TOOLFORGE_CONFIG"] = args.config

    from .config import default_config_path, load_config

    config = load_config(args.config or default_config_path())
    host = args.host or config.server.host
    port = args.port or config.server.port

    import uvicorn

    # Import app factory so config path is honored via TOOLFORGE_CONFIG
    uvicorn.run(
        "app.main:app",
        host=host,
        port=port,
        reload=args.reload,
        factory=False,
    )


if __name__ == "__main__":
    main()
