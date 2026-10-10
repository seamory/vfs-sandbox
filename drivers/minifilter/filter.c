#include <fltKernel.h>
#include <ntstrsafe.h>
#include "protocol.h"

#define TAG 'sfVV'
#define MAX_PROCESSES 4096
static PFLT_FILTER Filter;
static PFLT_PORT ServerPort, ClientPort;
static KSPIN_LOCK ProcessLock;
static PEPROCESS Processes[MAX_PROCESSES];
static ULONG ProcessCount;
static WCHAR UpperBuffer[VFS_PATH_CHARS];
static UNICODE_STRING Upper;
static BOOLEAN Registered;
typedef struct _VFS_HANDLE_CONTEXT { BOOLEAN Private; } VFS_HANDLE_CONTEXT;
DRIVER_INITIALIZE DriverEntry;
static FLT_POSTOP_CALLBACK_STATUS PostCreate(PFLT_CALLBACK_DATA Data,PCFLT_RELATED_OBJECTS Objects,PVOID CompletionContext,FLT_POST_OPERATION_FLAGS Flags) {
 VFS_HANDLE_CONTEXT *context; NTSTATUS status;
 UNREFERENCED_PARAMETER(Flags);
 if(!NT_SUCCESS(Data->IoStatus.Status) || Data->IoStatus.Status==STATUS_REPARSE)return FLT_POSTOP_FINISHED_PROCESSING;
 status=FltAllocateContext(Filter,FLT_STREAMHANDLE_CONTEXT,sizeof(*context),NonPagedPoolNx,(PFLT_CONTEXT*)&context);
 if(NT_SUCCESS(status)) {
  context->Private=((ULONG_PTR)CompletionContext==1);
  status=FltSetStreamHandleContext(Objects->Instance,Objects->FileObject,FLT_SET_CONTEXT_REPLACE_IF_EXISTS,context,NULL);
  FltReleaseContext(context);
 }
 if(!NT_SUCCESS(status)) {FltCancelFileOpen(Objects->Instance,Objects->FileObject);Data->IoStatus.Status=status;Data->IoStatus.Information=0;}
 return FLT_POSTOP_FINISHED_PROCESSING;
}


static BOOLEAN Tracked(PEPROCESS Process) {
 ULONG i; BOOLEAN found = FALSE; KIRQL oldIrql;
 KeAcquireSpinLock(&ProcessLock,&oldIrql);
 for (i=0;i<ProcessCount;i++) if (Processes[i]==Process) {found=TRUE;break;}
 KeReleaseSpinLock(&ProcessLock,oldIrql);
 return found;
}

static VOID ProcessNotify(PEPROCESS Process,HANDLE ProcessId,PPS_CREATE_NOTIFY_INFO Info) {
 ULONG i; KIRQL oldIrql; UNREFERENCED_PARAMETER(ProcessId);
 KeAcquireSpinLock(&ProcessLock,&oldIrql);
 if (Info) {
  // CreatingThreadId follows the actual creator even with a spoofed parent PID.
  for(i=0;i<ProcessCount;i++) {
   if (PsGetProcessId(Processes[i])==Info->CreatingThreadId.UniqueProcess) {
    if(ProcessCount==MAX_PROCESSES) {Info->CreationStatus=STATUS_INSUFFICIENT_RESOURCES;break;}
    ObReferenceObject(Process); Processes[ProcessCount++]=Process; break;
   }
  }
 } else {
  for(i=0;i<ProcessCount;i++) if(Processes[i]==Process) {
   ObDereferenceObject(Processes[i]); Processes[i]=Processes[--ProcessCount];break;
  }
 }
 KeReleaseSpinLock(&ProcessLock,oldIrql);
}

static BOOLEAN UnderUpper(PCUNICODE_STRING Name) {
 return Upper.Length && RtlPrefixUnicodeString(&Upper,Name,TRUE) &&
  (Name->Length==Upper.Length || Name->Buffer[Upper.Length/sizeof(WCHAR)]==L'\\');
}
static NTSTATUS CopyPath(WCHAR *Destination, PCUNICODE_STRING Source) {
 if(Source->Length>=VFS_PATH_CHARS*sizeof(WCHAR))return STATUS_NAME_TOO_LONG;
 RtlCopyMemory(Destination,Source->Buffer,Source->Length);
 Destination[Source->Length/sizeof(WCHAR)]=0;return STATUS_SUCCESS;
}
static NTSTATUS Exchange(ULONG Operation,ULONG Flags,PCUNICODE_STRING Path,
 PCUNICODE_STRING Target,VFS_REPLY *Reply) {
 VFS_REQUEST *request; NTSTATUS status; ULONG size=sizeof(*Reply); LARGE_INTEGER timeout;
 if(!ClientPort || !Registered)return STATUS_DEVICE_NOT_READY;
 request=ExAllocatePool2(POOL_FLAG_PAGED,sizeof(*request),TAG);
 if(!request)return STATUS_INSUFFICIENT_RESOURCES;
 request->Version=VFS_VERSION;request->Operation=Operation;request->Flags=Flags;
 request->ProcessId=HandleToULong(PsGetCurrentProcessId());
 status=CopyPath(request->Path,Path);
 if(NT_SUCCESS(status)&&Target)status=CopyPath(request->Target,Target);
 if(NT_SUCCESS(status)) {
  timeout.QuadPart=-300LL*10000000; // bounded; no allow-on-timeout path
  status=FltSendMessage(Filter,&ClientPort,request,sizeof(*request),Reply,&size,&timeout);
  if(status==STATUS_TIMEOUT)status=STATUS_IO_TIMEOUT;
  if(NT_SUCCESS(status)) {
   if(size!=sizeof(*Reply))status=STATUS_INVALID_BUFFER_SIZE;
   else if(Reply->Path[VFS_PATH_CHARS-1]!=0)status=STATUS_NAME_TOO_LONG;
   else status=Reply->Status;
  }
 }
 ExFreePoolWithTag(request,TAG);return status;
}
static FLT_PREOP_CALLBACK_STATUS Fail(PFLT_CALLBACK_DATA Data,NTSTATUS Status) {
 Data->IoStatus.Status=Status;Data->IoStatus.Information=0;return FLT_PREOP_COMPLETE;
}
static NTSTATUS NameOf(PFLT_CALLBACK_DATA Data,PFLT_FILE_NAME_INFORMATION *Name) {
 return FltGetFileNameInformation(Data,FLT_FILE_NAME_NORMALIZED|FLT_FILE_NAME_QUERY_DEFAULT,Name);
}
static FLT_PREOP_CALLBACK_STATUS PreCreate(PFLT_CALLBACK_DATA Data,PCFLT_RELATED_OBJECTS Objects,PVOID *Context) {
 NTSTATUS status;PFLT_FILE_NAME_INFORMATION name=NULL;VFS_REPLY *reply;ULONG flags=0,options,disposition;
 ACCESS_MASK access;UNICODE_STRING destination;
 if(!Tracked(PsGetCurrentProcess()))return FLT_PREOP_SUCCESS_NO_CALLBACK;
 if(KeGetCurrentIrql()!=PASSIVE_LEVEL)return Fail(Data,STATUS_ACCESS_DENIED);
 options=Data->Iopb->Parameters.Create.Options;
 if(Objects->Transaction)return Fail(Data,STATUS_NOT_SUPPORTED);
 if(options&FILE_OPEN_BY_FILE_ID)return Fail(Data,STATUS_NOT_SUPPORTED);
 // Never allow volume/device handles or named-pipe opens through this backend.
 if(!Objects->FileObject || Objects->FileObject->FileName.Length==0)return Fail(Data,STATUS_ACCESS_DENIED);
 status=FltGetFileNameInformation(Data,FLT_FILE_NAME_OPENED|FLT_FILE_NAME_QUERY_DEFAULT,&name);if(!NT_SUCCESS(status))return Fail(Data,status);
 access=Data->Iopb->Parameters.Create.SecurityContext->DesiredAccess;
 disposition=options>>24;
 if((access&(FILE_WRITE_DATA|FILE_APPEND_DATA|FILE_WRITE_EA|FILE_WRITE_ATTRIBUTES|DELETE|WRITE_DAC|WRITE_OWNER|GENERIC_WRITE|GENERIC_ALL|MAXIMUM_ALLOWED)) ||
    disposition!=FILE_OPEN || (options&FILE_DELETE_ON_CLOSE))flags|=VFS_WRITABLE;
 if(options&FILE_DIRECTORY_FILE)flags|=VFS_DIRECTORY;
 reply=ExAllocatePool2(POOL_FLAG_PAGED,sizeof(*reply),TAG);
 if(!reply){FltReleaseFileNameInformation(name);return Fail(Data,STATUS_INSUFFICIENT_RESOURCES);}
 status=Exchange(VFS_RESOLVE,flags,&name->Name,NULL,reply);
 if(NT_SUCCESS(status)) {
  RtlInitUnicodeString(&destination,reply->Path);
  if(!destination.Length)status=STATUS_INVALID_PARAMETER;
  else if(!RtlEqualUnicodeString(&destination,&name->Name,TRUE)) {
   status=IoReplaceFileObjectName(Objects->FileObject,destination.Buffer,destination.Length);
   if(NT_SUCCESS(status)) {
    Data->IoStatus.Status=STATUS_REPARSE;Data->IoStatus.Information=IO_REPARSE;
    FltSetCallbackDataDirty(Data);
    ExFreePoolWithTag(reply,TAG);FltReleaseFileNameInformation(name);return FLT_PREOP_COMPLETE;
   }
  }
 }
 *Context=(PVOID)(ULONG_PTR)(UnderUpper(&name->Name)?1:2);
 ExFreePoolWithTag(reply,TAG);FltReleaseFileNameInformation(name);
 if(!NT_SUCCESS(status))return Fail(Data,status);
 return FLT_PREOP_SUCCESS_WITH_CALLBACK;
}

static FLT_PREOP_CALLBACK_STATUS PreOperation(PFLT_CALLBACK_DATA Data,PCFLT_RELATED_OBJECTS Objects,PVOID *Context) {
 NTSTATUS status;PFLT_FILE_NAME_INFORMATION name=NULL,target=NULL;VFS_REPLY *reply;
 UCHAR major=Data->Iopb->MajorFunction;FILE_INFORMATION_CLASS cls;ULONG flags=0;VFS_HANDLE_CONTEXT *handleContext=NULL;BOOLEAN privateHandle=FALSE;
 UNREFERENCED_PARAMETER(Context);
 if(!Tracked(PsGetCurrentProcess()))return FLT_PREOP_SUCCESS_NO_CALLBACK;
 if(FLT_IS_FASTIO_OPERATION(Data))return FLT_PREOP_DISALLOW_FASTIO;
 if(KeGetCurrentIrql()>APC_LEVEL)return Fail(Data,STATUS_ACCESS_DENIED);
 if(!ClientPort || !Registered)return Fail(Data,STATUS_DEVICE_NOT_READY);
 // Arbitrary FSCTLs include raw writes, extent cloning and reparse manipulation.
 if(major==IRP_MJ_FILE_SYSTEM_CONTROL || major==IRP_MJ_DEVICE_CONTROL || major==IRP_MJ_SET_VOLUME_INFORMATION)return Fail(Data,STATUS_NOT_SUPPORTED);
 if(major==IRP_MJ_ACQUIRE_FOR_SECTION_SYNCHRONIZATION) {
  if(Data->Iopb->Parameters.AcquireForSectionSynchronization.SyncType!=SyncTypeCreateSection ||
     !(Data->Iopb->Parameters.AcquireForSectionSynchronization.PageProtection&(PAGE_READWRITE|PAGE_EXECUTE_READWRITE)))
   return FLT_PREOP_SUCCESS_NO_CALLBACK;
 }
 if(major==IRP_MJ_SET_INFORMATION &&
    (Data->Iopb->Parameters.SetFileInformation.FileInformationClass==FilePositionInformation ||
     Data->Iopb->Parameters.SetFileInformation.FileInformationClass==FileModeInformation))return FLT_PREOP_SUCCESS_NO_CALLBACK;
 status=FltGetStreamHandleContext(Objects->Instance,Objects->FileObject,(PFLT_CONTEXT*)&handleContext);
 if(NT_SUCCESS(status)){privateHandle=handleContext->Private;FltReleaseContext(handleContext);}
 else {
  // Deliberately inherited standard-output handles were opened by the broker.
  if(KeGetCurrentIrql()!=PASSIVE_LEVEL)return Fail(Data,STATUS_ACCESS_DENIED);
  status=NameOf(Data,&name);if(!NT_SUCCESS(status))return Fail(Data,status);
  privateHandle=UnderUpper(&name->Name);FltReleaseFileNameInformation(name);name=NULL;
 }
 if(!privateHandle)return Fail(Data,STATUS_ACCESS_DENIED);
 if(major!=IRP_MJ_DIRECTORY_CONTROL && major!=IRP_MJ_SET_INFORMATION)return FLT_PREOP_SUCCESS_NO_CALLBACK;
 if(KeGetCurrentIrql()!=PASSIVE_LEVEL)return Fail(Data,STATUS_ACCESS_DENIED);
 status=NameOf(Data,&name);if(!NT_SUCCESS(status))return Fail(Data,status);
 if(major==IRP_MJ_DIRECTORY_CONTROL) {
  if(Data->Iopb->MinorFunction!=IRP_MN_QUERY_DIRECTORY){FltReleaseFileNameInformation(name);return Fail(Data,STATUS_NOT_SUPPORTED);}
  reply=ExAllocatePool2(POOL_FLAG_PAGED,sizeof(*reply),TAG);
  if(!reply){FltReleaseFileNameInformation(name);return Fail(Data,STATUS_INSUFFICIENT_RESOURCES);}
  status=Exchange(VFS_LIST,0,&name->Name,NULL,reply);ExFreePoolWithTag(reply,TAG);
 } else if(major==IRP_MJ_SET_INFORMATION) {
  cls=Data->Iopb->Parameters.SetFileInformation.FileInformationClass;
  if(cls==FileRenameInformation) {
   PFILE_RENAME_INFORMATION rename=Data->Iopb->Parameters.SetFileInformation.InfoBuffer;
   if(Data->Iopb->Parameters.SetFileInformation.Length<FIELD_OFFSET(FILE_RENAME_INFORMATION,FileName) ||
      rename->FileNameLength>Data->Iopb->Parameters.SetFileInformation.Length-FIELD_OFFSET(FILE_RENAME_INFORMATION,FileName))status=STATUS_INVALID_PARAMETER;
   else {
    status=FltGetDestinationFileNameInformation(Objects->Instance,Objects->FileObject,rename->RootDirectory,
      rename->FileName,rename->FileNameLength,FLT_FILE_NAME_NORMALIZED|FLT_FILE_NAME_QUERY_DEFAULT,&target);
    if(NT_SUCCESS(status)) {
     reply=ExAllocatePool2(POOL_FLAG_PAGED,sizeof(*reply),TAG);
     if(!reply)status=STATUS_INSUFFICIENT_RESOURCES;
     else {if(rename->ReplaceIfExists)flags|=VFS_REPLACE;
      status=Exchange(VFS_RENAME,flags,&name->Name,&target->Name,reply);ExFreePoolWithTag(reply,TAG);}
     FltReleaseFileNameInformation(target);
    }
   }
   FltReleaseFileNameInformation(name);return Fail(Data,status);
  }
  // Restrict mutations to supported in-place operations. In particular, hard
  // links, extended renames and short-name assignment cannot escape the layer.
  if(cls!=FileBasicInformation && cls!=FileDispositionInformation && cls!=FileDispositionInformationEx &&
     cls!=FileEndOfFileInformation && cls!=FileAllocationInformation && cls!=FilePositionInformation)
   status=STATUS_NOT_SUPPORTED;
 }
 FltReleaseFileNameInformation(name);
 if(!NT_SUCCESS(status))return Fail(Data,status);
 return FLT_PREOP_SUCCESS_NO_CALLBACK;
}

static NTSTATUS Connect(PFLT_PORT Port,PVOID Cookie,PVOID Context,ULONG Size,PVOID *ConnectionCookie) {
 KIRQL oldIrql;
 UNREFERENCED_PARAMETER(Cookie);UNREFERENCED_PARAMETER(Context);UNREFERENCED_PARAMETER(Size);UNREFERENCED_PARAMETER(ConnectionCookie);
 KeAcquireSpinLock(&ProcessLock,&oldIrql);
 if(ProcessCount || ClientPort){KeReleaseSpinLock(&ProcessLock,oldIrql);return STATUS_DEVICE_BUSY;}
 Registered=FALSE;ClientPort=Port;
 KeReleaseSpinLock(&ProcessLock,oldIrql);return STATUS_SUCCESS;
}
static VOID Disconnect(PVOID Cookie) {
 UNREFERENCED_PARAMETER(Cookie);
 // Keep tracked processes until their exit notifications: disconnect must NOT
 // convert their future requests into unrestricted host I/O.
 Registered=FALSE;FltCloseClientPort(Filter,&ClientPort);
}
static NTSTATUS Message(PVOID Cookie,PVOID Input,ULONG InputSize,PVOID Output,ULONG OutputSize,PULONG Returned) {
 VFS_CONTROL *control;PEPROCESS process;NTSTATUS status;SIZE_T length;KIRQL oldIrql;
 UNREFERENCED_PARAMETER(Cookie);UNREFERENCED_PARAMETER(Output);UNREFERENCED_PARAMETER(OutputSize);*Returned=0;
 if(InputSize!=sizeof(VFS_CONTROL))return STATUS_INVALID_PARAMETER;
 control=ExAllocatePool2(POOL_FLAG_NON_PAGED,sizeof(*control),TAG);if(!control)return STATUS_INSUFFICIENT_RESOURCES;
 __try {RtlCopyMemory(control,Input,sizeof(*control));} __except(EXCEPTION_EXECUTE_HANDLER){ExFreePoolWithTag(control,TAG);return GetExceptionCode();}
 if(control->Version!=VFS_VERSION || control->Operation!=VFS_REGISTER || Registered || control->Upper[VFS_PATH_CHARS-1]!=0){ExFreePoolWithTag(control,TAG);return STATUS_INVALID_PARAMETER;}
 status=RtlStringCchLengthW(control->Upper,VFS_PATH_CHARS,&length);
 if(!NT_SUCCESS(status)||length<10 || control->Upper[length-1]==L'\\'){ExFreePoolWithTag(control,TAG);return STATUS_INVALID_PARAMETER;}
 status=PsLookupProcessByProcessId(ULongToHandle(control->ProcessId),&process);
 if(NT_SUCCESS(status)) {
  KeAcquireSpinLock(&ProcessLock,&oldIrql);
  if(ProcessCount){status=STATUS_DEVICE_BUSY;ObDereferenceObject(process);}
  else {
   RtlCopyMemory(UpperBuffer,control->Upper,sizeof(UpperBuffer));RtlInitUnicodeString(&Upper,UpperBuffer);
   Processes[ProcessCount++]=process;Registered=TRUE;
  }
  KeReleaseSpinLock(&ProcessLock,oldIrql);
 }
 ExFreePoolWithTag(control,TAG);return status;
}
static NTSTATUS InstanceSetup(PCFLT_RELATED_OBJECTS Objects,FLT_INSTANCE_SETUP_FLAGS Flags,DEVICE_TYPE Type,FLT_FILESYSTEM_TYPE FileSystem) {
 UNREFERENCED_PARAMETER(Objects);UNREFERENCED_PARAMETER(Flags);UNREFERENCED_PARAMETER(Type);UNREFERENCED_PARAMETER(FileSystem);
 // Attach to all filesystem instances so unsupported filesystems fail closed
 // for tracked processes rather than silently bypassing the filter.
 return STATUS_SUCCESS;
}
static NTSTATUS InstanceQueryTeardown(PCFLT_RELATED_OBJECTS Objects,FLT_INSTANCE_QUERY_TEARDOWN_FLAGS Flags) {
 UNREFERENCED_PARAMETER(Objects);UNREFERENCED_PARAMETER(Flags);
 return (ProcessCount || ClientPort)?STATUS_FLT_DO_NOT_DETACH:STATUS_SUCCESS;
}
static NTSTATUS Unload(FLT_FILTER_UNLOAD_FLAGS Flags) {
 UNREFERENCED_PARAMETER(Flags);
 if(ProcessCount || ClientPort)return STATUS_FLT_DO_NOT_DETACH;
 FltCloseCommunicationPort(ServerPort);PsSetCreateProcessNotifyRoutineEx(ProcessNotify,TRUE);FltUnregisterFilter(Filter);
 return STATUS_SUCCESS;
}
static FLT_PREOP_CALLBACK_STATUS PreNetworkQuery(PFLT_CALLBACK_DATA Data,PCFLT_RELATED_OBJECTS Objects,PVOID *Context) {
 UNREFERENCED_PARAMETER(Data);UNREFERENCED_PARAMETER(Objects);UNREFERENCED_PARAMETER(Context);
 return Tracked(PsGetCurrentProcess())?FLT_PREOP_DISALLOW_FASTIO:FLT_PREOP_SUCCESS_NO_CALLBACK;
}
static const FLT_CONTEXT_REGISTRATION Contexts[]={
 {FLT_STREAMHANDLE_CONTEXT,0,NULL,sizeof(VFS_HANDLE_CONTEXT),TAG,NULL,NULL,NULL},
 {FLT_CONTEXT_END}
};
static const FLT_OPERATION_REGISTRATION Operations[]={
 {IRP_MJ_CREATE,0,PreCreate,PostCreate},
 {IRP_MJ_NETWORK_QUERY_OPEN,0,PreNetworkQuery,NULL},
 {IRP_MJ_WRITE,0,PreOperation,NULL},
 {IRP_MJ_SET_INFORMATION,0,PreOperation,NULL},
 {IRP_MJ_SET_EA,0,PreOperation,NULL},
 {IRP_MJ_SET_SECURITY,0,PreOperation,NULL},
 {IRP_MJ_DIRECTORY_CONTROL,0,PreOperation,NULL},
 {IRP_MJ_FILE_SYSTEM_CONTROL,0,PreOperation,NULL},
 {IRP_MJ_DEVICE_CONTROL,0,PreOperation,NULL},
 {IRP_MJ_SET_VOLUME_INFORMATION,0,PreOperation,NULL},
 {IRP_MJ_ACQUIRE_FOR_SECTION_SYNCHRONIZATION,0,PreOperation,NULL},
 {IRP_MJ_OPERATION_END}
};
static const FLT_REGISTRATION Registration={
 sizeof(FLT_REGISTRATION),FLT_REGISTRATION_VERSION,FLTFL_REGISTRATION_SUPPORT_NPFS_MSFS,Contexts,Operations,Unload,
 InstanceSetup,InstanceQueryTeardown,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL
};
NTSTATUS DriverEntry(PDRIVER_OBJECT Driver,PUNICODE_STRING RegistryPath) {
 NTSTATUS status;PSECURITY_DESCRIPTOR security=NULL;OBJECT_ATTRIBUTES attributes;UNICODE_STRING name=RTL_CONSTANT_STRING(VFS_PORT_NAME);
 UNREFERENCED_PARAMETER(RegistryPath);KeInitializeSpinLock(&ProcessLock);
 status=FltRegisterFilter(Driver,&Registration,&Filter);if(!NT_SUCCESS(status))return status;
 status=PsSetCreateProcessNotifyRoutineEx(ProcessNotify,FALSE);if(!NT_SUCCESS(status)){FltUnregisterFilter(Filter);return status;}
 status=FltBuildDefaultSecurityDescriptor(&security,FLT_PORT_ALL_ACCESS);
 if(NT_SUCCESS(status)) {
  InitializeObjectAttributes(&attributes,&name,OBJ_KERNEL_HANDLE|OBJ_CASE_INSENSITIVE,NULL,security);
  status=FltCreateCommunicationPort(Filter,&ServerPort,&attributes,NULL,Connect,Disconnect,Message,1);
  FltFreeSecurityDescriptor(security);
 }
 if(NT_SUCCESS(status))status=FltStartFiltering(Filter);
 if(!NT_SUCCESS(status)) {
  if(ServerPort)FltCloseCommunicationPort(ServerPort);
  PsSetCreateProcessNotifyRoutineEx(ProcessNotify,TRUE);FltUnregisterFilter(Filter);
 }
 return status;
}
