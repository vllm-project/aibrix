from typing import Protocol, List, Union
import json
import logging

from optimizer import GPUProfile

logger = logging.getLogger("aibrix.gpuoptimizer.profilereader")

class ProfileReader(Protocol):
    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""

class FileProfileReader:
    def __init__(self, filepath: str) -> None:
        self.filepath = filepath

    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""
        with open(self.filepath, 'r') as f:
            try:
                # Try parse as singal json
                profiles = json.load(f)
            except Exception as e:
                try:
                    # Try parse as list of json (jsonl)
                    profiles = []
                    for line in f:
                        if line.strip() == "":
                            continue
                        profiles.append(json.loads(line))
                except Exception as e:
                    logger.warning(f"Invalid profile file format, expected list or dict: {e}")

        if isinstance(profiles, dict):
            profiles = [profiles]
        elif not isinstance(profiles, list):
            logger.warning("Invalid profile file format, expected list or dict.")
        
        return [GPUProfile(**profile) for profile in profiles]
    
class RedisProfileReader:
    def __init__(self, redis_client, model_name: str, key_prefix: str = "aibrix:profile_%s_") -> None:
        self.client = redis_client
        self.key_prefix = key_prefix % (model_name)

    def read(self) -> List[GPUProfile]:
        """Read the next batch of records from the data source."""
        cursor = 0
        matching_keys = []
        while True:
            cursor, keys = self.client.scan(cursor=cursor, match=f"{self.key_prefix}*")
            for key in keys:
                matching_keys.append(key)
            if cursor == 0:
                break
        if len(matching_keys) == 0:
            logger.warning(f"No profiles matching {self.key_prefix}* found in Redis")
        
        # Retrieve the objects associated with the keys
        records = []
        for key in matching_keys:
            # Deserialize by json: dict[string]int
            profile_data = self.client.get(key)
            if profile_data is None:
                raise Exception(f"Failed to retrieve {key.decode()} from Redis.")
                
            # Deserialize by json: dict[string]int
            profile = json.loads(profile_data)
            if not isinstance(profile, dict):
                raise Exception(f"Profile is not a dictionary")

            records.append(GPUProfile(**profile))

        return records
