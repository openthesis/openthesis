from setuptools import setup, find_packages

setup(
    name="openthesis",
    version="0.1.0",
    description="OpenThesis Python SDK - open-source SDK for deterministic testing",
    long_description=open("README.md").read(),
    long_description_content_type="text/markdown",
    author="OpenThesis contributors",
    url="https://github.com/burntcarrot/openthesis",
    packages=find_packages(),
    python_requires=">=3.8",
    install_requires=[],  # zero external dependencies
    classifiers=[
        "Development Status :: 3 - Alpha",
        "Intended Audience :: Developers",
        "License :: OSI Approved :: MIT License",
        "Programming Language :: Python :: 3",
        "Programming Language :: Python :: 3.8",
        "Programming Language :: Python :: 3.9",
        "Programming Language :: Python :: 3.10",
        "Programming Language :: Python :: 3.11",
        "Programming Language :: Python :: 3.12",
        "Topic :: Software Development :: Testing",
    ],
)
