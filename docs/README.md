# Using Sphinx to build html web pages for AIBrix

## Environment setup
Make sure that your python conda environment is setup correctly. The following installs sphinx package and necessary templates.

```bash
pip install -r requirements-docs.txt
```

## Compile html pages

```
make html
```

Now the html pages should be generated at "docs/build/html/index.html". You can open this html page with your web browser as our project front page.

Chinese catalogs are under `source/locale/zh_CN`. After English RST changes:

```
make update-po
```

Preview Chinese HTML at `docs/build/zh-cn`:

```
make html-zh
```

To exercise the English / 中文 navbar switcher locally, serve the `docs/build` directory over HTTP (opening `file://` pages also works after the switcher fix, but an HTTP server matches Read the Docs more closely):

```
python -m http.server -d build 8000
```

Then open `http://127.0.0.1:8000/html/` and `http://127.0.0.1:8000/zh-cn/`.

### Read the Docs Chinese project

Hosted Chinese pages need a separate Read the Docs project (same repo, language **Chinese Simplified**) linked from the main project's **Translations** settings. After that project builds successfully, set `AIBRIX_DOCS_SHOW_ZH=1` in the main project's RTD environment variables so the navbar offers 中文 on English pages.
