k8s_yaml('./.deploy/local.yaml')
docker_build('trickler', '.', dockerfile='Dockerfile')
k8s_resource('trickler-app', port_forwards="8080:80")
k8s_resource('statsd', port_forwards=["8125:8125", "8081:80"])
