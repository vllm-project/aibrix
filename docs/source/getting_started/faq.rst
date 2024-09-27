.. _faq:

===
FAQ
===

FAQ - Installation
------------------

Failed to delete the AIBrix
^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. figure:: ../assets/images/delete-namespace-stuck-1.png
  :alt: aibrix-architecture-v1
  :width: 70%
  :align: center

.. figure:: ../assets/images/delete-namespace-stuck-2.png
  :alt: aibrix-architecture-v1
  :width: 70%
  :align: center

In this case, you just need to find the model adapter, edit the object, and remove the ``finalizer`` pair. the pod would be deleted automatically.

