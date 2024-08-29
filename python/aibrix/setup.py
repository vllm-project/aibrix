
"""A setuptools based setup module.
"""
#!/usr/bin/env python

import io
import os

# Always prefer setuptools over distutils
from setuptools import find_packages, setup

# meta info for pypi package
NAME = "aibrix"
DESCRIPTION = "LLM inference project"
URL = "https://github.com/aibrix"

here = os.path.abspath(os.path.dirname(__file__))

about: dict[str, str] = {}
with io.open(os.path.join(here, NAME, "__version__.py")) as f:
    exec(f.read(), about)

setup(
    name='aibrix',
    version=about["__version__"],
    packages=find_packages(),  # Automatically find and include all packages
    install_requires=[
        # List your project's dependencies here
    ],
)