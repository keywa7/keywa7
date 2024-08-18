FROM python:3.12.3-alpine3.19

RUN apk upgrade
RUN apk add git g++ go

RUN <<EOF
git clone https://github.com/keywa7/keywa7 /keywa7
cd /keywa7/server
go build
EOF

EXPOSE 9999/tcp

CMD /keywa7/server/server --lport 9999 --lhost 0.0.0.0
