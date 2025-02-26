FROM golang:1.16 AS builder

ARG TARGETOS=linux
ARG TARGETARCH

ADD . /litmus-go
WORKDIR /litmus-go

RUN export GOOS=${TARGETOS} && \
    export GOARCH=${TARGETARCH}

RUN CGO_ENABLED=0 go build -o /output/experiments ./bin/experiment
RUN CGO_ENABLED=0 go build -o /output/helpers ./bin/helper


FROM alpine:3.13

LABEL maintainer="LitmusChaos"

ARG USER=litmus

ENV GLIBC_REPO=https://github.com/sgerrand/alpine-pkg-glibc
ENV GLIBC_VERSION=2.30-r0
ARG TARGETARCH

RUN rm -rf /var/lib/apt/lists/*

# Install generally useful things
RUN apk --update add \
        sudo \
	htop\
        bash\
        make\
        git \
        curl\
        iproute2\
        stress-ng\
        openssh-client\
#        libc6-compat \
        sshpass

RUN set -ex && \
     apk --update add libstdc++ curl ca-certificates && \
     for pkg in glibc-${GLIBC_VERSION} glibc-bin-${GLIBC_VERSION}; \
         do curl -sSL ${GLIBC_REPO}/releases/download/${GLIBC_VERSION}/${pkg}.apk -o /tmp/${pkg}.apk; done && \
     apk add --allow-untrusted /tmp/*.apk && \
     rm -v /tmp/*.apk && \
     /usr/glibc-compat/sbin/ldconfig /lib /usr/glibc-compat/lib

# Change default shell from ash to bash
RUN sed -i -e "s/bin\/ash/bin\/bash/" /etc/passwd   

#Installing Kubectl
ENV KUBE_LATEST_VERSION="v1.18.0"
RUN curl -L https://storage.googleapis.com/kubernetes-release/release/${KUBE_LATEST_VERSION}/bin/linux/${TARGETARCH}/kubectl -o     /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

#Installing crictl binaries
RUN curl -L https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.16.0/crictl-v1.16.0-linux-${TARGETARCH}.tar.gz --output crictl-v1.16.0-linux-${TARGETARCH}.tar.gz && \
    tar zxvf crictl-v1.16.0-linux-${TARGETARCH}.tar.gz -C /usr/local/bin
    
#Installing pumba binaries
ENV PUMBA_VERSION="0.7.7"
RUN curl -L https://github.com/alexei-led/pumba/releases/download/${PUMBA_VERSION}/pumba_linux_${TARGETARCH} --output /usr/local/bin/pumba && chmod +x /usr/local/bin/pumba

#Installing promql cli binaries
RUN curl -L https://github.com/litmuschaos/test-tools/releases/download/1.13.6/promql-linux-${TARGETARCH} --output /usr/local/bin/promql && chmod +x /usr/local/bin/promql

#Installing nsutil cli binaries
RUN curl -L https://github.com/litmuschaos/test-tools/releases/download/1.13.6/nsutil-linux-${TARGETARCH} --output /usr/local/bin/nsutil && chmod +x /usr/local/bin/nsutil

#Installing pause cli binaries
RUN curl -L https://github.com/litmuschaos/test-tools/releases/download/1.13.6/pause-linux-${TARGETARCH} --output /usr/local/bin/pause && chmod +x /usr/local/bin/pause

#Installing dns_interceptor cli binaries
RUN curl -L https://github.com/litmuschaos/test-tools/releases/download/1.13.6/dns_interceptor --output /usr/local/bin/dns_interceptor && chmod +x /usr/local/bin/dns_interceptor

# Installing memory-leak
RUN curl -L https://github.com/ojaswa1942/test-tools/raw/master/custom/memory-leak/memory-leak-${TARGETARCH} --output /usr/local/bin/memory-leak && chmod +x /usr/local/bin/memory-leak

COPY --from=docker:19.03 /usr/local/bin/docker /usr/local/bin/
COPY --from=builder /output/ /litmus

#Copying Necessary Files
COPY ./pkg/cloud/aws/common/ssm-docs/LitmusChaos-AWS-SSM-Docs.yml ./litmus/LitmusChaos-AWS-SSM-Docs.yml

#add new user
RUN adduser -D -S $USER \
        && echo "$USER ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/$USER \
        && chmod 0440 /etc/sudoers.d/$USER

USER $USER
WORKDIR /litmus
