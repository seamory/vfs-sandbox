# escape=`
# Native Windows containers only. See README.md for the required EWDK media.
ARG WINDOWS_BASE=mcr.microsoft.com/windows/servercore:ltsc2022
FROM ${WINDOWS_BASE}
SHELL ["cmd", "/S", "/C"]

# Extract the complete EWDK ISO contents here before building this image.
COPY ewdk/ C:/EWDK/
COPY entrypoint.cmd C:/build/entrypoint.cmd
COPY build-container.ps1 C:/build/build-container.ps1
RUN if not exist C:\EWDK\BuildEnv\SetupBuildEnv.cmd (echo Missing EWDK BuildEnv\SetupBuildEnv.cmd & exit /b 1)
WORKDIR C:/build
ENTRYPOINT ["cmd.exe", "/D", "/S", "/C", "C:\\build\\entrypoint.cmd"]
