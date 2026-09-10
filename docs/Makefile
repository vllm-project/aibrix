# Minimal makefile for Sphinx documentation
#

# You can set these variables from the command line, and also
# from the environment for the first two.
SPHINXOPTS    ?=
SPHINXBUILD   ?= sphinx-build
SOURCEDIR     = source
BUILDDIR      = build

# Put it first so that "make" without argument is like "make help".
help:
	@$(SPHINXBUILD) -M help "$(SOURCEDIR)" "$(BUILDDIR)" $(SPHINXOPTS) $(O)

html-zh:
	@$(SPHINXBUILD) -b html -D language=zh_CN "$(SOURCEDIR)" "$(BUILDDIR)/zh-cn" $(SPHINXOPTS) $(O)

update-po:
	@$(SPHINXBUILD) -M gettext "$(SOURCEDIR)" "$(BUILDDIR)" $(SPHINXOPTS) $(O)
	sphinx-intl update -p "$(BUILDDIR)/gettext" -d "$(SOURCEDIR)/locale" -l zh_CN

.PHONY: help html-zh update-po Makefile

# Catch-all target: route all unknown targets to Sphinx using the new
# "make mode" option.  $(O) is meant as a shortcut for $(SPHINXOPTS).
%: Makefile
	@$(SPHINXBUILD) -M $@ "$(SOURCEDIR)" "$(BUILDDIR)" $(SPHINXOPTS) $(O)
