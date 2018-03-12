FROM scratch
COPY  service-discovery-agent-unix /service-discovery-agent
ENTRYPOINT ["/service-discovery-agent"]
