# Deploying AIBrix with xDiT

sudo docker build -f docker/Dockerfile -t xdit-dev:latest .
TAG=v0.28
sudo docker tag xdit-dev:latest aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/xdit-dev:$TAG
sudo docker push aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/xdit-dev:$TAG

# Prerequisites

# Image Generation

```
curl -v "http://localhost:6000/generate" \
        -H "Content-Type: application/json" \
        -d '{
           "prompt": "Create a highly detailed digital painting of a futuristic cityscape at sunset, viewed from the perspective of standing on a glass skybridge between two skyscrapers. In the foreground, show three people: 1) A woman in a glowing cyberpunk exosuit holding a holographic umbrella. 2) A child wearing a fox mask and carrying a red balloon. 3) An old man with a robotic arm sketching the skyline in a notebook. The midground should feature flying cars with neon trails weaving between buildings, and a massive holographic billboard showing a koi fish swimming in mid-air. The background must have layered skyscrapers with different architectural styles: some with gothic spires, some with minimalist glass facades, and one central tower shaped like a spiraling helix. The lighting should mix warm golden sunlight with cold blue and pink neon reflections. Add atmospheric haze and lens flare to enhance depth. The overall style should blend Studio Ghibli softness, Syd Meads futuristic realism, and Moebius surreal linework. Ensure the composition feels cinematic, with the focus on the trio in the foreground while still capturing the overwhelming scale and vibrancy of the city.",
           "model": "hunyuan-dit",
           "save_disk_path": "/shared",
           "height": 1024,
           "width": 1024
         }'
```
# Video Generation