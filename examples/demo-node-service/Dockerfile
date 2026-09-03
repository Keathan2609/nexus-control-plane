# A workload for NEXUS to deploy. Kept minimal on purpose.
FROM node:22-alpine
WORKDIR /app
COPY package.json server.js ./
ARG VERSION=1.0.0
ENV VERSION=${VERSION}
USER node
EXPOSE 3000
CMD ["node", "server.js"]
