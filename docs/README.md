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

Now the html paged should be generated at "docs/build/html/index.html". You can open this html page with your web browser as our project front page.

Chinese catalogs are under `source/locale/zh_CN`. After English RST changes:

```
make update-po
```

Preview Chinese HTML at `docs/build/zh-cn`:

```
make html-zh
```
