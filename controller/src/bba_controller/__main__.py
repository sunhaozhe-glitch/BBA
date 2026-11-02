"""Standalone stdio entry point; startup arguments cannot be changed over MCP."""
import argparse
import asyncio
from pathlib import Path

from .controller import Settings
from .server import serve


def main():
    parser = argparse.ArgumentParser(description='BBA development controller phase 1 (stdio MCP)')
    parser.add_argument('--state-dir', type=Path, required=True)
    parser.add_argument('--repository', type=Path, required=True)
    parser.add_argument('--fixture-repository', type=Path)
    parser.add_argument('--authorize-fixture', action='store_true')
    parser.add_argument('--mode', choices=['OBSERVE','DEVELOPMENT'], default='OBSERVE')
    parser.add_argument('--authorize-development', action='store_true')
    parser.add_argument('--slots', type=int, choices=[1,2], default=1)
    args = parser.parse_args()
    fixture = args.authorize_fixture and args.fixture_repository is not None
    if args.authorize_fixture != (args.fixture_repository is not None):
        parser.error('fixture requires both exact repository and startup authorization')
    if fixture and args.fixture_repository.absolute() != args.repository.absolute():
        parser.error('fixture repository must exactly match configured repository')
    settings = Settings(args.state_dir, args.repository, mode=args.mode, fixture_authorized=fixture,
                        slots=args.slots, development_authorized=args.authorize_development)
    asyncio.run(serve(settings))


if __name__ == '__main__':
    main()
