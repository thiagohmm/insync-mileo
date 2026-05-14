#!/bin/bash

set -e

echo "Compilando insync-server..."
go build -o insync-server ./cmd/server

echo "Compilando insync-cli..."
go build -o insync-cli ./cmd/cli

echo "Encerrando processos antigos..."
pkill -f insync-server || true
pkill -f insync-cli || true

echo "Limpando conexões ativas (ports 50051, 8080)..."
lsof -ti:50051,8080 | xargs kill -9 2>/dev/null || true

echo "Build concluído!"
echo "Binários gerados:"
ls -lh insync-server insync-cli
