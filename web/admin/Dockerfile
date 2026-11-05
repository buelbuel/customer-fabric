FROM oven/bun:1.3.3
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile
COPY . .
ENV API_URL=http://api:8080
USER bun
EXPOSE 5173
CMD ["bun", "run", "dev"]
