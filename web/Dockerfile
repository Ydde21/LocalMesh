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
RUN adduser -D -u 10001 localmesh
COPY --from=build --chown=localmesh /app/.next/standalone ./
COPY --from=build --chown=localmesh /app/.next/static ./.next/static
COPY --from=build --chown=localmesh /app/public ./public
USER localmesh
EXPOSE 3000
CMD ["node", "server.js"]
