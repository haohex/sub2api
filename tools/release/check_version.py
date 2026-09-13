#!/usr/bin/env python3
"""Reject a KlN sync PR that retained the old VERSION during conflict resolution."""
from pathlib import Path
import sys

from reconcile import source_base

if __name__ == '__main__':
    print(source_base(Path('backend/cmd/server/VERSION').read_text(), sys.argv[1]))
