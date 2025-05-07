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

from msgspec import msgpack

from . import BaseMarshaller, Marshaller


class StringSerializer(BaseMarshaller):
    def __init__(self, marshaller: Marshaller | None = None) -> None:
        super().__init__(marshaller)
        self._encoder = msgpack.Encoder()
        self._decoder = msgpack.Decoder(str)

    def _marshal(self, data: str) -> bytes:
        return self._encoder.encode(data)

    def _unmarshal(self, data: bytes) -> str:
        return self._decoder.decode(data)
