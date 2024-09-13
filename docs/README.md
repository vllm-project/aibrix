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

Now the html paged should be generated at "website/docs/build/html/index.html". You can open this html page with your web browser as our project front page. 
