# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
import collections.abc
import copy


def merge_yaml_object(base, overlay, copy_on_write=True):
    """
    Recursively merges two YAML objects, mimicking kustomize's strategic merge.
    Accepts both dictionaries and Kubernetes API objects as input.
    """
    base_dict = base.to_dict() if hasattr(base, "to_dict") else base
    overlay_dict = overlay.to_dict() if hasattr(overlay, "to_dict") else overlay

    merged = base_dict
    if copy_on_write:
        merged = copy.deepcopy(base_dict)

    for key, value in overlay_dict.items():
        if (
            key in merged
            and isinstance(merged[key], collections.abc.Mapping)
            and isinstance(value, collections.abc.Mapping)
        ):
            merged[key] = merge_yaml_object(merged[key], value, False)

        elif (
            key in merged and isinstance(merged[key], list) and isinstance(value, list)
        ):
            # To merge a list, we use "name" field as the key
            base_list = merged[key]
            overlay_list = value
            strategy_merge = False

            # Create a map of base items by their 'name' for quick lookups
            base_items_by_name = {
                item.get("name"): item
                for item in base_list
                if isinstance(item, collections.abc.Mapping) and "name" in item
            }
            strategy_merge = len(base_items_by_name) > 0

            for item in overlay_list:
                if isinstance(item, collections.abc.Mapping) and "name" in item:
                    item_name = item.get("name")
                    if item_name in base_items_by_name:
                        # If an item with the same name exists, merge them
                        base_item = base_items_by_name[item_name]
                        base_items_by_name[item_name] = merge_yaml_object(
                            base_item, item, False
                        )
                    else:
                        # Otherwise, append the new item
                        base_items_by_name[item_name] = item
                        base_list.append(item)
                else:
                    # If the overlay item isn't a dict with a name, just append it
                    base_list.append(item)

            if strategy_merge:
                merged[key] = list(base_items_by_name.values())
            else:
                merged[key] = base_list
        else:
            merged[key] = value

    return merged
