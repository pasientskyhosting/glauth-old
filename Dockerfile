FROM alpine:3.15

COPY bin/glauth64      /app/glauth64
COPY sample-simple.cfg /data/glauth.cfg
COPY certs/*  /data/

EXPOSE 636
EXPOSE 5555

VOLUME [ "/data" ]
CMD [ "/app/glauth64", "-c", "/data/glauth.cfg" ]