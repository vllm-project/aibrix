# Using Sphinx to build html web pages for AIBrix

## Environment setup
Make sure that your python conda environment is setup correctly. The following installs sphinx package and necessary templates.

```bash
pip install -r requirements-docs.txt
```

## Compile HTML pages

```
make html-all
```

The English pages are generated at `docs/build/html`, and the Chinese pages
are generated at `docs/build/html/zh-cn`. Use `make html` when only the English
pages are needed, or `make html-zh` to rebuild only the Chinese pages.

Chinese catalogs are under `source/locale/zh_CN`. After English RST changes:

```
make update-po
```

Preview Chinese HTML at `docs/build/html/zh-cn`:

```
make html-zh
```

To exercise the English / 中文 navbar switcher locally, serve the
`docs/build/html` directory over HTTP:

```
python3 -m http.server -d build/html 8000
```

Then open `http://127.0.0.1:8000/` and
`http://127.0.0.1:8000/zh-cn/`.

Run the language switcher path tests from the repository root:

```
node --test docs/tests/language-switcher.test.js
```

### Read the Docs deployment

The Read the Docs build publishes both languages from the same project. English
is served from the version root, such as `/latest/`, and Chinese is nested at
`/latest/zh-cn/`. No separate translation project or environment variable is
required.
