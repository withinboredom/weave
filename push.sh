MY_DOCKER_REGISTRY=swytch.azurecr.io
for image in weave weaveexec weave-kube weave-npc weavedb network-tester; do 
    sudo docker tag weaveworks/$image:latest $MY_DOCKER_REGISTRY/weaveworks/$image:latest
    sudo docker push $MY_DOCKER_REGISTRY/weaveworks/$image:latest
done
