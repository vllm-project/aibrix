.. _release:

=======
Release
=======

.. note::
    This document is for release team only. Feel free to skip it.

This process outlines the steps required to create and publish a release for AIBrix Github project.
Follow these steps to ensure a smooth and consistent release cycle.

Prepare the code
----------------

Option 1 RC version release
^^^^^^^^^^^^^^^^^^^^^^^^^^^

For RC release like ``v0.2.0-rc.2``, there's no need to checkout a new branch, Let's cut the tag & release
directly against ``main`` branch.


Option 2 minor version release
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

For new minor version release like ``v0.1.0``, please checkout a new branch named ``release-0.1``.

.. code-block:: bash

    git checkout main && git fetch origin main && git rebase origin/main
    git checkout -b release-0.1 # cut from main branch
    git push origin release-0.1

.. note::
    Here we assume ``origin`` points to upstream, if it doesn't, other remotes like ``upstream`` should be right remote to push to.

Option 3: patch version release
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Bug fixes should be merged on ``main`` first. Then cherry-pick the bugfix to target release branch like ``release-0.1``.
Due to ``main`` changes, the fix may not able to be cherry-picked to ``release-0.1``. If that's the case, cut PR to release branch directly.
For patch version like ``v0.1.1``, please reuse the release branch ``release-0.1``, it should be created earlier from the minor version release.
for patch release, we do not rebase ``main`` because it will introduce new features. All fixes have to be cherry-picked or cut PR against ``release-0.1`` directly.

Cut a PR
--------

Make sure the manifest images tags and updated and python version is updated. A sample PR is `Cut v0.1.0-rc.5 release <https://github.com/aibrix/aibrix/pull/376>`_.
Merge the PR.

.. note::
    container image actually is not built yet, we preserve the tag name and it would be built later once the tag is created.


Create the tag and push to remote
---------------------------------

.. code-block:: bash

    # make sure you fetch the earlier PR locally
    git fetch origin release-0.1
    git rebase origin/release-0.1

    # create the tag
    git tag v0.1.0

    # push the tag
    git push origin v0.1.0

Monitor the release pipeline
----------------------------

After pushing the tag, the release pipeline (e.g., CI/CD workflows) should automatically begin. This may include:
- Running tests and validations
- Building manifest artifacts
- Building container image and push to the registry
- Building python library and upload to PyPI

Monitor the pipeline's progress to ensure it completes successfully

.. figure:: ../assets/images/release-pipeline-manifests.png
  :alt: draft-release
  :width: 70%
  :align: center

.. figure:: ../assets/images/release-pipeline-python-package.png
  :alt: draft-release
  :width: 70%
  :align: center

Publish the release on Github
-----------------------------

Release pipeline will cut a draft pre-release in `Github Releases <https://github.com/aibrix/aibrix/releases>`_.
Go to the "Releases" section in the repository, select the draft release corresponding to the tag you created.
Include release notes summarizing the changes (new features, bug fixes, breaking changes, etc.).
Optionally attach binaries, documentation, or other assets. In the end, let's publish the release.

.. figure:: ../assets/images/draft-release.png
  :alt: draft-release
  :width: 70%
  :align: center

Sync images to Volcano Engine Container Registry
------------------------------------------------

Currently, release pipeline only push images to dockerhub. In order to use them in VKE,
we need to retag the images and push to VKE Container Registry.

.. note::
    It requires you to use a machine that have both VKE and Dockerhub access.
    Do not forget to get the temporary credential and login the registry service before pushing.

.. code-block:: bash

    ./hack/release/sync-images.sh v0.2.1 aibrix-container-registry-cn-beijing.cr.volces.com
    ./hack/release/sync-images.sh v0.2.1 aibrix-container-registry-cn-shanghai.cr.volces.com


Update released tags in main branch docs
----------------------------------------

A sample PR is `here <https://github.com/aibrix/aibrix/pull/378>`_.
