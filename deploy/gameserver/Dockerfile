# Your game's dedicated server: a Godot 4 .NET Linux export of its server project,
# named gameserver.x86_64 and dropped into build/ before `compose build`.
# The export is self-contained (the .NET runtime ships in data_gameserver_linuxbsd_x86_64/),
# so the base only needs the native libraries .NET loads: ICU, OpenSSL, zlib.
FROM mcr.microsoft.com/dotnet/runtime-deps:9.0

COPY build/ /opt/gameserver/
# An export copied from Windows loses its execute bit on the way.
RUN chmod 755 /opt/gameserver/gameserver.x86_64

# Same uid as the distroless services, so the service_keys mount (owned by 65532)
# is readable. Godot's user:// lives under $HOME.
ENV HOME=/tmp
USER 65532:65532
WORKDIR /opt/gameserver

# Launch options come from OTOMO_GS_* and OTOMO_ALLOCATOR_* (docs/guide/game-servers.md
# lists them and what the server must do with them).
ENTRYPOINT ["/opt/gameserver/gameserver.x86_64", "--headless", "--"]
