# LocalMesh dashboard (Next.js standalone build).

FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM node:22-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3000 HOSTNAME=0.0.0.0
# The runtime serves `node server.js` — the bundled npm CLI (and its
# vuln-laden dep tree under /usr/local/lib/node_modules/npm) is unused
# here and ships CVEs the release scan must not carry. Strip it plus
# corepack and the bin shims; apk upgrade pulls patched base packages.
RUN apk upgrade --no-cache \
    && rm -rf /usr/local/lib/node_modules/npm \
    && rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
    && adduser -D -u 10001 localmesh
COPY --from=build --chown=localmesh /app/.next/standalone ./
COPY --from=build --chown=localmesh /app/.next/static ./.next/static
COPY --from=build --chown=localmesh /app/public ./public
USER localmesh
EXPOSE 3000
CMD ["node", "server.js"]
