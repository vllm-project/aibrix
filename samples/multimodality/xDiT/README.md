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
Here we assumes the image we build is `aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/xdit-dev:customized-v1`.

Sample deployment files are under `image-generation` directory. Each assumes the targetd models have been pre-downloaded on the `/data01/models` of the hosted machine's path. You could tune the path as needed. 

Apply the following deployment file to your cluster.

```
kubectl apply -f xDiT-intergation/xdit-dev.yaml
```

```
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80
```

```
curl -v "http://localhost:8888/v1/image/generations" \
        -H "Content-Type: application/json" \
        -d '{
           "prompt": "Create a highly detailed digital painting of a futuristic cityscape at sunset, viewed from the perspective of standing on a glass skybridge between two skyscrapers. In the foreground, show three people: 1) A woman in a glowing cyberpunk exosuit holding a holographic umbrella. 2) A child wearing a fox mask and carrying a red balloon. 3) An old man with a robotic arm sketching the skyline in a notebook. The midground should feature flying cars with neon trails weaving between buildings, and a massive holographic billboard showing a koi fish swimming in mid-air. The background must have layered skyscrapers with different architectural styles: some with gothic spires, some with minimalist glass facades, and one central tower shaped like a spiraling helix. The lighting should mix warm golden sunlight with cold blue and pink neon reflections. Add atmospheric haze and lens flare to enhance depth. The overall style should blend Studio Ghibli softness, Syd Meads futuristic realism, and Moebius surreal linework. Ensure the composition feels cinematic, with the focus on the trio in the foreground while still capturing the overwhelming scale and vibrancy of the city.",
           "model": "hunyuan-dit",
           "save_disk_path": "/shared",
           "height": 1024,
           "width": 1024
         }'
```
## Video Generation