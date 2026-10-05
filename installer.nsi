; WildLink installer — build with: makensis installer.nsi
Unicode true
!include "MUI2.nsh"
!include "x64.nsh"

!define APP       "WildLink"
!define TITLE     "Wild Link"
!define VERSION   "2.1.0"
!define PUBLISHER "Wildchild Studios"
!define EXE       "WildLink.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP}"

Name "Wild Link"
OutFile "WildLink-Setup.exe"
InstallDir "$PROGRAMFILES64\${APP}"
InstallDirRegKey HKLM "${UNINSTKEY}" "InstallLocation"
RequestExecutionLevel admin
SetCompressor /SOLID lzma
BrandingText "${APP} ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APP}"
VIAddVersionKey "FileDescription" "${APP} Setup"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "CompanyName" "${PUBLISHER}"
VIAddVersionKey "LegalCopyright" "${PUBLISHER}"

!define MUI_ICON   "wildlink.ico"
!define MUI_UNICON "wildlink.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TEXT "This will install ${APP} ${VERSION}, the office messenger for chatting and sharing photos, videos and files with colleagues on the same network.$\r$\n$\r$\nIf LAN Chat or an older ${APP} is running, setup will close it for you.$\r$\n$\r$\nClick Next to continue."
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Open ${APP} now"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchAsUser

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

; Launch un-elevated (via Explorer) so the app runs as the signed-in user, not as admin.
Function LaunchAsUser
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\${EXE}"'
FunctionEnd

Section "Install"
  ; Close any running copy (including the older LAN Chat build) so files can be replaced.
  nsExec::Exec 'taskkill /F /IM ${EXE}'
  nsExec::Exec 'taskkill /F /IM LANChat.exe'
  nsExec::Exec 'taskkill /F /IM WildChat.exe'
  Sleep 800

  SetOutPath "$INSTDIR"
  File "WildLink.exe"
  File "wildlink.ico"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; Start menu (searchable from Windows Search) and desktop shortcuts for all users
  SetShellVarContext all
  CreateShortcut "$SMPROGRAMS\${TITLE}.lnk" "$INSTDIR\${EXE}" "" "$INSTDIR\wildlink.ico" 0
  CreateShortcut "$DESKTOP\${TITLE}.lnk"    "$INSTDIR\${EXE}" "" "$INSTDIR\wildlink.ico" 0

  ; Firewall: allow WildLink on private and work networks so there's no prompt on first run
  nsExec::Exec 'netsh advfirewall firewall delete rule name="${APP}"'
  nsExec::Exec 'netsh advfirewall firewall add rule name="${APP}" dir=in action=allow program="$INSTDIR\${EXE}" enable=yes profile=any'

  ; Apps & features / Control Panel entry
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayName"     "${TITLE}"
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayVersion"  "${VERSION}"
  WriteRegStr   HKLM "${UNINSTKEY}" "Publisher"       "${PUBLISHER}"
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayIcon"     "$INSTDIR\wildlink.ico"
  WriteRegStr   HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr   HKLM "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "EstimatedSize" 6200

  ; Remove the old LAN Chat auto-start entry
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "LANChat"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "WildChat"
SectionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill /F /IM ${EXE}'
  Sleep 800
  SetShellVarContext all
  Delete "$SMPROGRAMS\${TITLE}.lnk"
  Delete "$SMPROGRAMS\WildChat.lnk"
  Delete "$DESKTOP\${TITLE}.lnk"
  Delete "$DESKTOP\WildChat.lnk"
  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\wildlink.ico"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  nsExec::Exec 'netsh advfirewall firewall delete rule name="${APP}"'
  DeleteRegKey HKLM "${UNINSTKEY}"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${APP}"

  SetShellVarContext current
  MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also delete your WildLink chat history and settings?$\r$\n$\r$\nFiles you received in Downloads\WildLink Files are kept either way." /SD IDNO IDNO keep
    RMDir /r "$APPDATA\${APP}"
  keep:
SectionEnd
