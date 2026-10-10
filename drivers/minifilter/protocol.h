#pragma once
// Wire ABI v1: little-endian, fixed-width fields, no pointers. Keep in sync with
// run_windows_minifilter.go. All paths are absolute NT device paths, no NULs.
// The same wire format is used by x86 and x64, including a WOW64 broker.
#define VFS_VERSION 1
#define VFS_PATH_CHARS 2048
#define VFS_PORT_NAME L"\\VfsSandboxPort"
#define VFS_REGISTER 1
#define VFS_RESOLVE 2
#define VFS_LIST 3
#define VFS_RENAME 4
#define VFS_WRITABLE 1
#define VFS_DIRECTORY 2
#define VFS_REPLACE 4

typedef struct _VFS_CONTROL {
 ULONG Version, Operation, ProcessId, Reserved;
 WCHAR Upper[VFS_PATH_CHARS];
} VFS_CONTROL;
typedef struct _VFS_REQUEST {
 ULONG Version, Operation, Flags, ProcessId;
 WCHAR Path[VFS_PATH_CHARS];
 WCHAR Target[VFS_PATH_CHARS];
} VFS_REQUEST;
typedef struct _VFS_REPLY {
 NTSTATUS Status;
 ULONG Reserved;
 WCHAR Path[VFS_PATH_CHARS];
} VFS_REPLY;
C_ASSERT(sizeof(VFS_CONTROL) == 4112);
C_ASSERT(sizeof(VFS_REQUEST) == 8208);
C_ASSERT(sizeof(VFS_REPLY) == 4104);
C_ASSERT(FIELD_OFFSET(VFS_CONTROL, Upper) == 16);
C_ASSERT(FIELD_OFFSET(VFS_REQUEST, Path) == 16);
C_ASSERT(FIELD_OFFSET(VFS_REQUEST, Target) == 4112);
C_ASSERT(FIELD_OFFSET(VFS_REPLY, Path) == 8);
