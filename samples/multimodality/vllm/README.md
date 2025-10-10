# Deploying AIBrix with vLLM



Apply [Qwen-VL](https://huggingface.co/Qwen/Qwen2.5-VL-7B-Instruct) model


```
kubectl apply -f vllm/qwen-vl.yaml 
```


Forward AIBrix port

```
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80
```

Sending image QA request based on image URL

```
curl -v http://localhost:8888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-vl",
    "messages": [
      {
                "role": "user",
                "content": [
                    {"type": "text", "text": "What are the animals in these images?"},
                    {
                        "type": "image_url",
                        "image_url": {"url": "https://qianwen-res.oss-cn-beijing.aliyuncs.com/Qwen-VL/assets/demo.jpeg"}
                    }
                ]
            }
    ]
  }'
  
```

An output would look like

```
* Host localhost:8888 was resolved.
* IPv6: ::1
* IPv4: 127.0.0.1
*   Trying [::1]:8888...
* Connected to localhost (::1) port 8888
> POST /v1/chat/completions HTTP/1.1
> Host: localhost:8888
> User-Agent: curl/8.7.1
> Accept: */*
> Content-Type: application/json
> Content-Length: 451
> 
* upload completely sent off: 451 bytes
< HTTP/1.1 200 OK
< date: Fri, 10 Oct 2025 02:53:23 GMT
< server: uvicorn
< content-type: application/json
< x-went-into-req-headers: true
< request-id: de09e57d-a98f-4c9b-98f5-1f7425289977
< transfer-encoding: chunked
< 
* Connection #0 to host localhost left intact
{"id":"chatcmpl-233dc076-9044-4c75-9cad-142004caa379","object":"chat.completion","created":1760064805,"model":"qwen-vl","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":null,"content":"The image shows a dog, specifically a Labrador Retriever, interacting with a person on a beach. The dog is wearing a harness and appears to be giving a high-five to the person.","tool_calls":[]},"logprobs":null,"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":3606,"total_tokens":3646,"completion_tokens":40,"prompt_tokens_details":null},"prompt_logprobs":null,"kv_transfer_params":null}
```

Sending image QA request based on pod-local image path

```
# upload
kubectl cp demo.jpeg qwen-vl-64d666ffbc-thq9n:/models/demo.jpeg

# request
curl -v http://localhost:8888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-vl",
    "messages": [
      {
                "role": "user",
                "content": [
                    {"type": "text", "text": "What are the animals in these images?"},
                    {
                        "type": "image_url",
                        "image_url": {"url": "file:///models/demo.jpeg"}
                    }
                ]
            }
    ]
  }'

```

Sending image QA request based on encoded local image

```
python send_file_base64.py demo.jpeg --text "What
 are the animals in this image?"

```


Similarly, for video QA, you could send request like:

```
curl -v http://localhost:8888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-vl",
    "messages": [
      {
                "role": "user",
                "content": [
                    {"type": "text", "text": "What is in this video?"},
                    {
                        "type": "video_url",
                        "video_url": {"url": "https://test-videos.co.uk/vids/bigbuckbunny/mp4/h264/360/Big_Buck_Bunny_360_10s_1MB.mp4"}
                    }
                ]
            }
    ]
  }'
  
```

A sample output would look like:

```
* Host localhost:8888 was resolved.
* IPv6: ::1
* IPv4: 127.0.0.1
*   Trying [::1]:8888...
* Connected to localhost (::1) port 8888
> POST /v1/chat/completions HTTP/1.1
> Host: localhost:8888
> User-Agent: curl/8.7.1
> Accept: */*
> Content-Type: application/json
> Content-Length: 451
> 
* upload completely sent off: 451 bytes
< HTTP/1.1 200 OK
< date: Fri, 10 Oct 2025 03:00:52 GMT
< server: uvicorn
< content-type: application/json
< x-went-into-req-headers: true
< request-id: 37791f66-a904-4355-90b5-b7a11abba077
< transfer-encoding: chunked
< 
* Connection #0 to host localhost left intact
{"id":"chatcmpl-1fa297ab-e198-47de-bd50-e5fdb2be7a77","object":"chat.completion","created":1760065256,"model":"qwen-vl","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":null,"content":"The video depicts a whimsical, animated scene set in a lush, green forest. The focal point of the scene is a tree that has grown directly out of a mound of moss-covered earth. The tree's roots are exposed and extend outward, creating a natural bridge or pathway over a small cave-like opening in the ground. The surrounding environment is vibrant with various shades of green, and there are other trees and foliage in the background, adding depth to the scene. The lighting suggests it is either early morning or late afternoon, with sunlight filtering through the leaves, casting dappled shadows on the ground. The overall atmosphere is serene and magical, evoking a sense of fantasy and enchantment.","tool_calls":[]},"logprobs":null,"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":4811,"total_tokens":4951,"completion_tokens":140,"prompt_tokens_details":null},"prompt_logprobs":null,"kv_transfer_params":null}

```

More examples could be found [here](https://github.com/vllm-project/aibrix/issues/1512).