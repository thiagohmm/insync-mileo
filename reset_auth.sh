#!/bin/bash

set -e

echo "🔄 Resetando autenticação do Insync..."
echo ""

# 1. Encerrar processos ativos
echo "1️⃣ Encerrando processos do insync..."
pkill -f insync-server || echo "   (servidor não estava rodando)"
pkill -f insync-cli || echo "   (CLI não estava rodando)"
sleep 1

# 2. Limpar portas
echo ""
echo "2️⃣ Limpando portas 50051 e 8080..."
lsof -ti:50051,8080 | xargs kill -9 2>/dev/null || echo "   (portas já estavam livres)"

# 3. Remover banco de dados
echo ""
echo "3️⃣ Removendo banco de dados antigo..."
if [ -f "insync.db" ]; then
    rm -f insync.db
    echo "   ✅ insync.db removido"
else
    echo "   (banco não existia)"
fi

# 4. Recompilar
echo ""
echo "4️⃣ Recompilando com a correção do refresh token..."
go build -o insync-server ./cmd/server
go build -o insync-cli ./cmd/cli
echo "   ✅ Compilação concluída"

# 5. Instruções
echo ""
echo "✅ Reset completo!"
echo ""
echo "📋 Próximos passos:"
echo "   1. Execute: ./insync-cli"
echo "   2. Cole o código de autenticação do Google"
echo "   3. A nova autenticação incluirá o refresh token"
echo ""
echo "💡 A correção aplicada adiciona 'access_type=offline' e 'prompt=consent'"
echo "   na URL OAuth2, garantindo que o Google forneça o refresh token."
