# syntax=docker/dockerfile:1.6

FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src

COPY GitProtect.sln ./
COPY GitProtect/GitProtect.csproj GitProtect/
COPY GitProtect.Client/GitProtect.Client.csproj GitProtect.Client/
RUN dotnet restore GitProtect/GitProtect.csproj

COPY . ./
RUN dotnet publish GitProtect/GitProtect.csproj -c Release -o /app/publish --no-restore

FROM mcr.microsoft.com/dotnet/aspnet:10.0 AS runtime
ARG VERSION=dev
ENV ASPNETCORE_URLS=http://0.0.0.0:3000 \
    PUID=1000 \
    PGID=1000 \
    TZ=UTC \
    VERSION=$VERSION

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /app/publish/ /app/
COPY entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh \
    && mkdir -p /data \
    && chmod 775 /data

VOLUME ["/data"]
EXPOSE 3000
ENTRYPOINT ["/entrypoint.sh"]
