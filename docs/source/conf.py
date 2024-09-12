# Configuration file for the Sphinx documentation builder.
#
# For the full list of built-in configuration values, see the documentation:
# https://www.sphinx-doc.org/en/master/usage/configuration.html

# -- Project information -----------------------------------------------------
# https://www.sphinx-doc.org/en/master/usage/configuration.html#project-information

project = 'AIBrix'
copyright = '2024, AIBrix Team'
author = 'the AIBrix Authors'

# -- General configuration ---------------------------------------------------
# https://www.sphinx-doc.org/en/master/usage/configuration.html#general-configuration

extensions = [
    'sphinxemoji.sphinxemoji',
    'sphinx.ext.autodoc',
    'sphinx.ext.autosummary',
    'sphinx.ext.duration',
    'sphinx.ext.doctest',
    'sphinx.ext.intersphinx',
    'sphinx.ext.napoleon',
    'sphinx.ext.viewcode',
    'sphinx_autodoc_typehints',
    'sphinx_click',
    'sphinx_copybutton',
    'sphinx_design',
    'myst_parser',
]

templates_path = ['_templates']
exclude_patterns = []

# Exclude the prompt "$" when copying code
copybutton_prompt_text = r"\$ "
copybutton_prompt_is_regexp = True


# -- Options for HTML output -------------------------------------------------
# https://www.sphinx-doc.org/en/master/usage/configuration.html#options-for-html-output

html_title = project
html_theme = 'sphinx_book_theme'
html_logo = 'assets/logos/aibrix-logo-light.png'
html_static_path = ['_static']
html_theme_options = {
    # repository level setting
    'repository_url': 'https://github.com/aibrix/aibrix',
    "use_repository_button": True,
    "repository_branch": "main",
    'path_to_docs': 'docs/source',

    # theme
    'pygment_light_style': 'tango',
    'pygment_dark_style': 'monokai',


    # navigation and sidebar
    'show_toc_level': 2,
    'announcement': None,
    'secondary_sidebar_items': [
        'page-toc',
    ],
    'navigation_depth': 3,
    'primary_sidebar_end': [],

    # article

    # footer
    'footer_start': [],
    'footer_center': [],
    'footer_end': [],
}

intersphinx_mapping = {
    "python": ("https://docs.python.org/3", None),
    "typing_extensions":
        ("https://typing-extensions.readthedocs.io/en/latest", None),
    "pillow": ("https://pillow.readthedocs.io/en/stable", None),
    "numpy": ("https://numpy.org/doc/stable", None),
    "torch": ("https://pytorch.org/docs/stable", None),
    "psutil": ("https://psutil.readthedocs.io/en/stable", None),
}
