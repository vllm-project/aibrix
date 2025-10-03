# Deploying AIBrix with xDiT
## Prerequisites
### Download xDiT and patch to xDiT
1. Download patch from [xDiT-intergation/xdit-52e74e88d2332281eefe68894af02f805a1d2b4f.patch](xDiT-intergation/xdit-52e74e88d2332281eefe68894af02f805a1d2b4f.patch). The following steps assume the patch will be in your current path and apply this patch automatically while using `Dockerfile`. 
2. Download xDiT source code from [xdit-project/xDiT](https://github.com/xdit-project/xDiT) and copy patch file.
```bash
# remove if xDiT exists
rm -r xDiT
git clone https://github.com/xdit-project/xDiT.git
cp xdit-52e74e88d2332281eefe68894af02f805a1d2b4f.patch xDiT/
cd xDiT && git checkout 52e74e88d2332281eefe68894af02f805a1d2b4f
git apply xdit-52e74e88d2332281eefe68894af02f805a1d2b4f.patch
```

### Build customized xDiT image
Assuming you're under the directory of `xDiT`, build your customized xDiT image like the following. 

```
TAG=customized-v1
REGISTRY="aibrix-container-registry-cn-beijing.cr.volces.com/aibrix"
IMAGE="xdit-dev"

sudo docker build -f docker/Dockerfile -t ${IMAGE}:latest .
sudo docker tag ${IMAGE}:latest ${REGISTRY}/${IMAGE}:${TAG}
sudo docker push ${REGISTRY}/${IMAGE}:${TAG}
```


## Image Generation
Here we assumes the image we build is `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/xdit-dev:customized-v1`. Sample deployment files are under `image-generation` directory. 

Apply the following deployment file to your cluster.

```
kubectl apply -f image-intergation/aibrix_vke_kv_image_sd_parallel.yaml
```

Forward AIBrix port

```
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80
```

```
curl -v "http://localhost:8888/v1/image/generations" \
        -H "Content-Type: application/json" \
        -d '{
           "prompt": "Create a highly detailed digital painting of a futuristic cityscape at sunset, viewed from the perspective of standing on a glass skybridge between two skyscrapers. In the foreground, show three people: 1) A woman in a glowing cyberpunk exosuit holding a holographic umbrella. 2) A child wearing a fox mask and carrying a red balloon. 3) An old man with a robotic arm sketching the skyline in a notebook. The midground should feature flying cars with neon trails weaving between buildings, and a massive holographic billboard showing a koi fish swimming in mid-air. The background must have layered skyscrapers with different architectural styles: some with gothic spires, some with minimalist glass facades, and one central tower shaped like a spiraling helix. The lighting should mix warm golden sunlight with cold blue and pink neon reflections. Add atmospheric haze and lens flare to enhance depth. The overall style should blend Studio Ghibli softness, Syd Meads futuristic realism, and Moebius surreal linework. Ensure the composition feels cinematic, with the focus on the trio in the foreground while still capturing the overwhelming scale and vibrancy of the city.",
           "model": "sd-3",
           "height": 1024,
           "width": 1024
         }'
```
Sample output looks like:
```
*   Trying 127.0.0.1:8888...
* Connected to localhost (127.0.0.1) port 8888 (#0)
> POST /v1/image/generations HTTP/1.1
> Host: localhost:8888
> User-Agent: curl/7.88.1
> Accept: */*
> Content-Type: application/json
> Content-Length: 1330
> 
< HTTP/1.1 200 OK
< date: Thu, 02 Oct 2025 22:45:05 GMT
< server: uvicorn
< content-type: application/json
< x-went-into-req-headers: true
< request-id: 104281eb-98de-4ffd-92e4-90e32118e028
< target-pod: 192.168.0.28:6000
< transfer-encoding: chunked
< 
* Connection #0 to host localhost left intact
{"message":"Image generated successfully","elapsed_time":"12.52 sec","output":"/shared/generated_image_20251002-224519.png","save_to_disk":true}
```

From the output we know that the image is saved to `/shared/generated_image_20251002-224519.png` with the request id `104281eb-98de-4ffd-92e4-90e32118e02`. You could use `/view` interface to download the image from AIBrix. For example:
```
curl -v http://localhost:8888/view \
  -H "Content-Type: application/json" \
  -d '{
    	"request-id": "104281eb-98de-4ffd-92e4-90e32118e028",
    	"path": "/shared/generated_image_20251002-224519.png"
  	}' -o generated_image_20251002-224519.png
```

You could also emit the `save_disk_path` in the request and the response will contains base64 encoded image. For example, you could use the following script to decode binary image to an image file. 

```python
import requests
import base64

# 1. Define the endpoint
url = "http://localhost:8888/v1/image/generations"

# 2. Define the payload
payload = {
    "prompt": "Create a highly detailed digital painting of a futuristic cityscape at sunset, viewed from the perspective of standing on a glass skybridge between two skyscrapers. In the foreground, show three people: 1) A woman in a glowing cyberpunk exosuit holding a holographic umbrella. 2) A child wearing a fox mask and carrying a red balloon. 3) An old man with a robotic arm sketching the skyline in a notebook. The midground should feature flying cars with neon trails weaving between buildings, and a massive holographic billboard showing a koi fish swimming in mid-air. The background must have layered skyscrapers with different architectural styles: some with gothic spires, some with minimalist glass facades, and one central tower shaped like a spiraling helix. The lighting should mix warm golden sunlight with cold blue and pink neon reflections. Add atmospheric haze and lens flare to enhance depth. The overall style should blend Studio Ghibli softness, Syd Meads futuristic realism, and Moebius surreal linework. Ensure the composition feels cinematic, with the focus on the trio in the foreground while still capturing the overwhelming scale and vibrancy of the city.",
    "model": "sd-3",
    "height": 1024,
    "width": 1024
}

# 3. Make the request
response = requests.post(url, json=payload)
response.raise_for_status()  # Raise an error if the request failed

# 4. Extract base64 image from the response
data = response.json()
image_base64 = data.get("image")  # Adjust key if your server uses a different key

# 5. Decode and save as PNG
if image_base64:
    image_bytes = base64.b64decode(image_base64)
    with open("output.png", "wb") as f:
        f.write(image_bytes)
    print("Image saved as output.png")
else:
    print("No image returned in response.")

```

## Video Generation

Apply a video model deployment file, for example:

```
kubectl apply -f video-generation/aibrix_vke_staging_video_cogvideo_parallel.yaml
```

Send a video generation request. For example:

```
curl -v "http://localhost:8888/v1/video/generations" \
        -H "Content-Type: application/json" \
        -H "routing-strategy: random" \
        -d '{
           "prompt": "Generate a 15-second cinematic 4K video of a futuristic floating city at sunset. The city features towering neon-lit skyscrapers with holographic advertisements, interconnected by glowing sky bridges. Flying vehicles zip through the air in intricate patterns, reflecting off glass surfaces. Include subtle fog and drifting clouds to enhance depth. The camera should start with a wide aerial shot, slowly panning down to street level, showing pedestrians with cybernetic augmentations interacting with autonomous robots. Incorporate dynamic lighting: the sun sets in the background casting long shadows, neon lights flicker, and reflections shimmer on wet streets. Add ambient environmental details: rain begins to fall, puddles ripple, leaves flutter in the breeze. Ensure smooth, natural motion for all objects, realistic physics for flying vehicles, and cinematic depth of field. Include soft orchestral music in the background that crescendos as the camera approaches street level. The overall mood is awe-inspiring, futuristic, and slightly mysterious.",
           "model": "cogvideox-2b",
           "num_inference_steps": 50,
           "save_disk_path": "/shared"
         }'
```

Sample output:

```
*   Trying 127.0.0.1:8888...
* Connected to localhost (127.0.0.1) port 8888 (#0)
> POST /v1/video/generations HTTP/1.1
> Host: localhost:8888
> User-Agent: curl/7.88.1
> Accept: */*
> Content-Type: application/json
> routing-strategy: random
> Content-Length: 1206
> 
< HTTP/1.1 200 OK
< date: Thu, 02 Oct 2025 23:37:59 GMT
< server: uvicorn
< content-type: application/json
< x-went-into-req-headers: true
< request-id: 693d63ba-936b-4c9a-a78d-a754aab0bf3a
< target-pod: 192.168.0.98:6000
< transfer-encoding: chunked
< 
* Connection #0 to host localhost left intact
{"message":"Video generated successfully","elapsed_time":"71.49 sec","output":"/shared/generated_video_20251002-233911.mp4","save_to_disk":true}

```


```
curl -v http://localhost:8888/view \
  -H "Content-Type: application/json" \
  -d '{
    	"request-id": "693d63ba-936b-4c9a-a78d-a754aab0bf3a",
    	"path": "/shared/generated_video_20251002-233911.mp4"
  	}' -o generated_video_20251002-233911.mp4

```