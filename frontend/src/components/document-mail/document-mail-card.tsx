import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, FileText, FlaskConical, Loader2, Save, TriangleAlert } from "lucide-react";
import { useForm, useWatch } from "react-hook-form";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useRBAC } from "@/hooks/use-rbac";
import { ApiError } from "@/lib/api-client";
import { permissions } from "@/lib/permissions";
import { cn } from "@/lib/utils";
import {
  documentMailKeys,
  getDocumentMailSetting,
  sendDocumentMailTest,
  streamDocumentMailProgress,
  updateDocumentMailSetting,
} from "@/services/document-mail";
import { toast } from "@/stores/toast-store";
import type {
  DocumentMailProgressEvent,
  DocumentMailProvider,
  DocumentMailSetting,
  DocumentMailTestResult,
  DocumentPdfStatus,
} from "@/types/admin";

const portOptions = [
  { value: "587", label: "587 — STARTTLS (disarankan)" },
  { value: "465", label: "465 — SSL/TLS" },
] as const;

const providerOptions: { value: DocumentMailProvider; label: string; description: string }[] = [
  {
    value: "gmail",
    label: "Gmail SMTP",
    description: "Kirim langsung melalui smtp.gmail.com dengan App Password.",
  },
  {
    value: "smtp_forge",
    label: "SMTP Forge",
    description: "Kirim melalui gateway HTTP SMTP Forge dengan X-Mailer-Api-Key.",
  },
];

const emailSchema = z.email();
const uuidSchema = z.uuid();

function isValidHttpUrl(value: string): boolean {
  try {
    const parsed = new URL(value);
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

function isAccountChange(
  current: DocumentMailSetting | undefined,
  username: string,
  port: string,
): boolean {
  if (!current?.has_smtp_password || isAccountRemoval(username)) {
    return false;
  }
  return (
    username.trim().toLowerCase() !== current.smtp_username || Number(port) !== current.smtp_port
  );
}

function isAccountRemoval(username: string): boolean {
  return username.trim() === "";
}

function isForgeUrlChange(current: DocumentMailSetting | undefined, baseUrl: string): boolean {
  const normalized = baseUrl.trim().replace(/\/+$/, "");
  if (!current?.has_smtp_forge_api_key || normalized === "") {
    return false;
  }
  return normalized !== (current.smtp_forge_base_url ?? "").trim().replace(/\/+$/, "");
}

function buildDocumentMailSchema(current: DocumentMailSetting | undefined) {
  return z
    .object({
      enabled: z.boolean(),
      provider: z.enum(["gmail", "smtp_forge"]),
      smtp_username: z
        .string()
        .trim()
        .max(160, "Maksimal 160 karakter")
        .refine(
          (value) => value === "" || emailSchema.safeParse(value).success,
          "Format email tidak valid",
        ),
      smtp_password: z.string().max(200, "Maksimal 200 karakter"),
      clear_smtp_password: z.boolean(),
      smtp_port: z.enum(["587", "465"]),
      sender_name: z.string().trim().max(120, "Maksimal 120 karakter"),
      smtp_forge_base_url: z
        .string()
        .trim()
        .max(255, "Maksimal 255 karakter")
        .refine(
          (value) => value === "" || isValidHttpUrl(value),
          "URL harus diawali http:// atau https://",
        ),
      smtp_forge_api_key: z.string().max(255, "Maksimal 255 karakter"),
      clear_smtp_forge_api_key: z.boolean(),
      smtp_forge_account_id: z
        .string()
        .trim()
        .refine(
          (value) => value === "" || uuidSchema.safeParse(value).success,
          "SMTP Account ID harus berupa UUID yang valid",
        ),
      smtp_forge_from_email: z
        .string()
        .trim()
        .max(160, "Maksimal 160 karakter")
        .refine(
          (value) => value === "" || emailSchema.safeParse(value).success,
          "Format email tidak valid",
        ),
    })
    .superRefine((values, ctx) => {
      if (values.provider === "gmail") {
        if (values.enabled && values.smtp_username === "") {
          ctx.addIssue({
            code: "custom",
            path: ["smtp_username"],
            message: "Alamat Gmail wajib diisi saat Email Dokumen diaktifkan",
          });
        }
        if (
          isAccountChange(current, values.smtp_username, values.smtp_port) &&
          values.smtp_password.trim() === ""
        ) {
          ctx.addIssue({
            code: "custom",
            path: ["smtp_password"],
            message:
              "Alamat Gmail atau port berubah: isi app password baru (password lama tidak dipakai untuk akun lain).",
          });
        }
      } else {
        if (values.enabled && values.smtp_forge_base_url.trim() === "") {
          ctx.addIssue({
            code: "custom",
            path: ["smtp_forge_base_url"],
            message: "URL SMTP Forge wajib diisi saat Email Dokumen diaktifkan",
          });
        }
        if (
          isForgeUrlChange(current, values.smtp_forge_base_url) &&
          values.smtp_forge_api_key.trim() === ""
        ) {
          ctx.addIssue({
            code: "custom",
            path: ["smtp_forge_api_key"],
            message:
              "URL SMTP Forge berubah: isi API key baru untuk server ini.",
          });
        }
      }
    });
}

type DocumentMailFormValues = z.infer<ReturnType<typeof buildDocumentMailSchema>>;

function toFormValues(setting: DocumentMailSetting | undefined): DocumentMailFormValues {
  return {
    enabled: setting?.enabled ?? false,
    provider: setting?.provider === "smtp_forge" ? "smtp_forge" : "gmail",
    smtp_username: setting?.smtp_username ?? "",
    smtp_password: "",
    clear_smtp_password: false,
    smtp_port: setting?.smtp_port === 465 ? "465" : "587",
    sender_name: setting?.sender_name ?? "",
    smtp_forge_base_url: setting?.smtp_forge_base_url ?? "",
    smtp_forge_api_key: "",
    clear_smtp_forge_api_key: false,
    smtp_forge_account_id: setting?.smtp_forge_account_id ?? "",
    smtp_forge_from_email: setting?.smtp_forge_from_email ?? "",
  };
}

const mailpitUIURL = "http://localhost:8025";

function isLocalCaptureAddr(addr: string): boolean {
  const host = addr.replace(/:\d+$/, "").replace(/^\[|\]$/g, "");
  return host === "localhost" || host === "127.0.0.1" || host === "::1";
}

function describePdfStatus(pdf: DocumentPdfStatus | undefined): {
  text: string;
  tone: "success" | "warning" | "muted";
} | null {
  if (!pdf) {
    return null;
  }
  if (pdf.enabled) {
    return pdf.source === "auto"
      ? { text: "aktif (LibreOffice terdeteksi otomatis)", tone: "success" }
      : { text: "aktif (LibreOffice dari SOFFICE_BIN)", tone: "success" };
  }
  switch (pdf.source) {
    case "disabled":
      return { text: "nonaktif (SOFFICE_BIN=off)", tone: "muted" };
    case "env_invalid":
      return {
        text: "nonaktif — SOFFICE_BIN tidak valid; perbaiki atau hapus nilainya agar LibreOffice dideteksi otomatis, lalu restart backend",
        tone: "warning",
      };
    default:
      return {
        text: "nonaktif — LibreOffice tidak ditemukan di server; pasang LibreOffice lalu restart backend",
        tone: "warning",
      };
  }
}

const inputClass =
  "h-10 rounded-[6px] border-transparent bg-surface-muted px-3 text-[14px] focus:border-ops focus:bg-surface focus:ring-2 focus:ring-ops/20";

type TestResultState =
  | { kind: "queued"; recipient: string; deliveryId: string; progress?: DocumentMailProgressEvent }
  | { kind: "sent"; recipient: string }
  | { kind: "error"; message: string }
  | null;

export function DocumentMailCard() {
  const queryClient = useQueryClient();
  const { hasPermission } = useRBAC();
  const canManage = hasPermission(permissions.adminSettingsManage);
  const [testResult, setTestResult] = useState<TestResultState>(null);
  const sseAbortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    return () => {
      sseAbortRef.current?.abort();
    };
  }, []);

  const settingQuery = useQuery({
    queryKey: documentMailKeys.setting(),
    queryFn: getDocumentMailSetting,
  });
  const setting = settingQuery.data;

  const schema = useMemo(() => buildDocumentMailSchema(setting), [setting]);
  const {
    control,
    formState: { errors },
    handleSubmit,
    register,
    reset,
    setError,
    setValue,
    getValues,
  } = useForm<DocumentMailFormValues>({
    resolver: zodResolver(schema),
    defaultValues: toFormValues(undefined),
  });

  useEffect(() => {
    if (setting) {
      reset(toFormValues(setting));
    }
  }, [setting, reset]);

  const enabled = useWatch({ control, name: "enabled" });
  const provider = useWatch({ control, name: "provider" });
  const clearPassword = useWatch({ control, name: "clear_smtp_password" });
  const typedPassword = useWatch({ control, name: "smtp_password" });
  const watchedUsername = useWatch({ control, name: "smtp_username" });
  const watchedPort = useWatch({ control, name: "smtp_port" });

  const watchedForgeUrl = useWatch({ control, name: "smtp_forge_base_url" });
  const typedForgeKey = useWatch({ control, name: "smtp_forge_api_key" });
  const clearForgeKey = useWatch({ control, name: "clear_smtp_forge_api_key" });

  const accountChanged = isAccountChange(setting, watchedUsername, watchedPort);
  const accountRemoved = isAccountRemoval(watchedUsername);
  const clearingPassword = clearPassword && !accountChanged;

  const forgeUrlChanged = isForgeUrlChange(setting, watchedForgeUrl);
  const forgeUrlEmpty = watchedForgeUrl.trim() === "";
  const clearingForgeKey = clearForgeKey && !forgeUrlChanged;

  const saveMutation = useMutation({
    mutationFn: updateDocumentMailSetting,
    onSuccess: async (saved) => {
      toast.success("Pengaturan Email Dokumen berhasil diperbarui");
      setTestResult(null);
      queryClient.setQueryData(documentMailKeys.setting(), saved);
      reset(toFormValues(saved));
      await queryClient.invalidateQueries({ queryKey: documentMailKeys.setting() });
    },
    onError: (error) => {
      if (error instanceof ApiError && error.code === "SMTP_PASSWORD_REQUIRED") {
        setError("smtp_password", { message: error.message });
      } else if (error instanceof ApiError && error.code === "SMTP_FORGE_API_KEY_REQUIRED") {
        setError("smtp_forge_api_key", { message: error.message });
      } else if (error instanceof ApiError && error.code === "VALIDATION_ERROR") {
        const details = (error.details ?? {}) as Record<string, string>;
        if (details.smtp_username) {
          setError("smtp_username", { message: "Alamat Gmail tidak valid atau belum diisi" });
        }
        if (details.smtp_forge_base_url) {
          setError("smtp_forge_base_url", { message: error.message });
        }
        if (details.smtp_forge_account_id) {
          setError("smtp_forge_account_id", { message: error.message });
        }
        if (details.smtp_forge_from_email) {
          setError("smtp_forge_from_email", { message: error.message });
        }
      }
      toast.error(
        "Gagal memperbarui Email Dokumen",
        error instanceof Error ? error.message : undefined,
      );
    },
  });

  const testMutation = useMutation({
    mutationFn: sendDocumentMailTest,
    onMutate: () => {
      sseAbortRef.current?.abort();
      setTestResult(null);
    },
    onSuccess: async (result: DocumentMailTestResult) => {
      if (!result.sent) {
        setTestResult({
          kind: "error",
          message: result.error_message ?? "Pengiriman email uji gagal",
        });
        return;
      }

      if (result.external_job_id) {
        setTestResult({
          kind: "queued",
          recipient: result.recipient,
          deliveryId: result.delivery_id,
          progress: { event: "progress", status: "queued", step: "queued", percentage: 0, message: "Menunggu antrean SMTP Forge..." },
        });
        const controller = new AbortController();
        sseAbortRef.current = controller;
        try {
          await streamDocumentMailProgress(
            `/admin/settings/document-mail/deliveries/${result.delivery_id}/progress`,
            (ev) => {
              if (ev.event === "completed") {
                setTestResult({ kind: "sent", recipient: result.recipient });
              } else if (ev.event === "failed") {
                setTestResult({
                  kind: "error",
                  message: ev.error || ev.message || "Pengiriman melalui SMTP Forge gagal",
                });
              } else {
                setTestResult({
                  kind: "queued",
                  recipient: result.recipient,
                  deliveryId: result.delivery_id,
                  progress: ev,
                });
              }
            },
            controller.signal,
          );
        } catch (streamErr) {
          if (!controller.signal.aborted) {
            setTestResult({
              kind: "error",
              message:
                streamErr instanceof Error
                  ? `Email masuk antrean, namun stream progres terputus: ${streamErr.message}`
                  : "Stream progres SMTP Forge terputus",
            });
          }
        }
      } else {
        setTestResult({ kind: "sent", recipient: result.recipient });
      }
    },
    onError: (error) => {
      setTestResult({
        kind: "error",
        message: error instanceof Error ? error.message : "Pengiriman email uji gagal",
      });
    },
  });

  const onSubmit = handleSubmit((values) => {
    const password = values.smtp_password.trim();
    const removal = isAccountRemoval(values.smtp_username);
    const clear =
      (removal && (setting?.has_smtp_password ?? false)) ||
      (values.clear_smtp_password &&
        !isAccountChange(setting, values.smtp_username, values.smtp_port));

    const forgeKey = values.smtp_forge_api_key.trim();
    const forgeEmpty = values.smtp_forge_base_url.trim() === "";
    const clearForge =
      (forgeEmpty && (setting?.has_smtp_forge_api_key ?? false)) ||
      (values.clear_smtp_forge_api_key &&
        !isForgeUrlChange(setting, values.smtp_forge_base_url));

    saveMutation.mutate({
      enabled: values.enabled,
      provider: values.provider,
      smtp_username: values.smtp_username.trim().toLowerCase(),
      smtp_password: clear || removal || password === "" ? null : password,
      clear_smtp_password: clear,
      smtp_port: values.smtp_port === "465" ? 465 : 587,
      sender_name: values.sender_name.trim(),
      smtp_forge_base_url: values.smtp_forge_base_url.trim().replace(/\/+$/, ""),
      smtp_forge_api_key: clearForge || forgeEmpty || forgeKey === "" ? null : forgeKey,
      clear_smtp_forge_api_key: clearForge,
      smtp_forge_account_id: values.smtp_forge_account_id.trim(),
      smtp_forge_from_email: values.smtp_forge_from_email.trim().toLowerCase(),
    });
  });

  const handleSendTest = () => {
    const values = getValues();
    testMutation.mutate({
      provider: values.provider,
      smtp_username: values.smtp_username.trim().toLowerCase(),
      smtp_password: values.smtp_password.trim() === "" ? null : values.smtp_password.trim(),
      smtp_port: values.smtp_port === "465" ? 465 : 587,
      sender_name: values.sender_name.trim(),
      smtp_forge_base_url: values.smtp_forge_base_url.trim().replace(/\/+$/, ""),
      smtp_forge_api_key:
        values.smtp_forge_api_key.trim() === "" ? null : values.smtp_forge_api_key.trim(),
      smtp_forge_account_id: values.smtp_forge_account_id.trim(),
      smtp_forge_from_email: values.smtp_forge_from_email.trim().toLowerCase(),
    });
  };

  const hasStoredPassword = setting?.has_smtp_password ?? false;
  const hasStoredForgeKey = setting?.has_smtp_forge_api_key ?? false;
  const devAddr =
    provider === "gmail" && setting?.delivery?.mode === "dev_capture"
      ? (setting.delivery.capture_addr ?? setting.dev_smtp_addr ?? null)
      : provider === "gmail"
        ? (setting?.dev_smtp_addr ?? null)
        : null;
  const pdfStatus = describePdfStatus(setting?.pdf);
  const disabled = !canManage || settingQuery.isLoading;

  const canTestCurrentForm = useMemo(() => {
    if (provider === "smtp_forge") {
      const validUrl = isValidHttpUrl(watchedForgeUrl.trim());
      const hasKey = typedForgeKey.trim() !== "" || (hasStoredForgeKey && !forgeUrlChanged && !clearingForgeKey);
      return validUrl && hasKey;
    }
    const validUser = emailSchema.safeParse(watchedUsername.trim()).success;
    const hasPass =
      typedPassword.trim() !== "" ||
      (hasStoredPassword && !accountChanged && !clearingPassword) ||
      Boolean(devAddr);
    return validUser && hasPass;
  }, [
    provider,
    watchedForgeUrl,
    typedForgeKey,
    hasStoredForgeKey,
    forgeUrlChanged,
    clearingForgeKey,
    watchedUsername,
    typedPassword,
    hasStoredPassword,
    accountChanged,
    clearingPassword,
    devAddr,
  ]);

  const passwordStatus = hasStoredPassword
    ? "Tersimpan"
    : devAddr
      ? "Tidak diperlukan (mode dev)"
      : "Belum ada";
  const passwordNote = accountRemoved
    ? hasStoredPassword || typedPassword.trim()
      ? "Alamat Gmail dikosongkan: app password tidak disimpan dan yang tersimpan akan dihapus saat disimpan."
      : "App password belum tersimpan untuk tenant ini."
    : accountChanged
      ? "Alamat Gmail atau port berubah: app password tersimpan akan dihapus, isi app password baru untuk akun ini."
      : clearingPassword
        ? "App password tersimpan akan dihapus saat disimpan."
        : typedPassword.trim()
          ? hasStoredPassword
            ? "App password baru akan menggantikan yang tersimpan saat disimpan."
            : "App password baru akan disimpan (terenkripsi) saat disimpan."
          : hasStoredPassword
            ? "App password sudah tersimpan (terenkripsi, tidak pernah ditampilkan). Isi lagi hanya jika ingin mengganti."
            : "App password belum tersimpan untuk tenant ini.";

  const forgeKeyNote = forgeUrlEmpty
    ? hasStoredForgeKey || typedForgeKey.trim()
      ? "URL SMTP Forge dikosongkan: API key tidak disimpan dan yang tersimpan akan dihapus saat disimpan."
      : "API key belum tersimpan untuk tenant ini."
    : forgeUrlChanged
      ? "URL SMTP Forge berubah: API key tersimpan akan dihapus, isi API key baru."
      : clearingForgeKey
        ? "API key tersimpan akan dihapus saat disimpan."
        : typedForgeKey.trim()
          ? hasStoredForgeKey
            ? "API key baru akan menggantikan yang tersimpan saat disimpan."
            : "API key baru akan disimpan (terenkripsi) saat disimpan."
          : hasStoredForgeKey
            ? "API key sudah tersimpan (terenkripsi, tidak pernah ditampilkan). Isi lagi hanya jika ingin mengganti."
            : "API key belum tersimpan untuk tenant ini.";

  return (
    <Card className="space-y-5 p-6 xl:col-span-2">
      <div className="flex items-start gap-3">
        <div className="rounded-md bg-error-light p-3 text-error">
          <FileText className="h-5 w-5" />
        </div>
        <div>
          <h2 className="font-display text-[18px] font-[700] text-text-primary">
            Email Dokumen
          </h2>
          <p className="mt-1 text-sm text-text-secondary">
            Konfigurasi pengiriman slip gaji, kontrak kerja, dan dokumen legal sebagai lampiran PDF.
            Reset kata sandi dan notifikasi tetap memakai Email Tenant di atas.
          </p>
        </div>
      </div>

      {devAddr ? (
        <div
          className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning-light px-4 py-3 text-sm text-text-primary"
          data-testid="document-mail-dev-capture"
        >
          <FlaskConical className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
          <p>
            <span className="font-semibold">
              Mode development: email dokumen tidak dikirim ke Gmail, tetapi ditangkap Mailpit di{" "}
              {devAddr}
            </span>
            {isLocalCaptureAddr(devAddr) ? (
              <>
                {" "}
                — buka{" "}
                <a
                  className="font-semibold text-ops underline"
                  href={mailpitUIURL}
                  rel="noreferrer"
                  target="_blank"
                >
                  {mailpitUIURL}
                </a>
              </>
            ) : null}
            <span className="text-text-secondary">
              {" "}
              (SMTP lokal tanpa TLS/login). App password tidak diperlukan.
            </span>
          </p>
        </div>
      ) : null}

      <form className="space-y-5" noValidate onSubmit={onSubmit}>
        <div className="grid gap-6 xl:grid-cols-[minmax(0,1.4fr)_minmax(280px,0.8fr)]">
          <div className="space-y-4">
            <div className="rounded-md border border-border bg-surface-muted/60 p-4">
              <label className="flex items-center justify-between gap-4">
                <div>
                  <p className="text-sm font-semibold text-text-primary">Aktifkan Email Dokumen</p>
                  <p className="text-xs text-text-secondary">
                    Saat nonaktif, slip gaji dan kontrak tidak bisa dikirim lewat email.
                  </p>
                </div>
                <input
                  className="h-5 w-5 accent-[var(--module-primary)]"
                  disabled={disabled}
                  type="checkbox"
                  {...register("enabled", { deps: ["smtp_username", "smtp_forge_base_url"] })}
                />
              </label>
            </div>

            <div className="space-y-2">
              <p className="text-[13px] font-[600] text-text-primary">Provider Pengiriman</p>
              <div className="grid gap-3 sm:grid-cols-2">
                {providerOptions.map((option) => {
                  const selected = provider === option.value;
                  return (
                    <button
                      className={cn(
                        "rounded-md border p-3 text-left transition-all",
                        selected
                          ? "border-ops bg-ops/5 ring-1 ring-ops"
                          : "border-border bg-surface hover:bg-surface-muted/60",
                        disabled && "cursor-not-allowed opacity-60",
                      )}
                      disabled={disabled}
                      key={option.value}
                      onClick={() => setValue("provider", option.value, { shouldDirty: true })}
                      type="button"
                    >
                      <p className="text-sm font-semibold text-text-primary">{option.label}</p>
                      <p className="mt-0.5 text-xs text-text-secondary">{option.description}</p>
                    </button>
                  );
                })}
              </div>
            </div>

            {provider === "gmail" ? (
              <>
                <div className="grid gap-4 md:grid-cols-2">
                  <Field
                    error={errors.smtp_username?.message}
                    htmlFor="document-mail-username"
                    label="Alamat Gmail"
                    required={enabled}
                  >
                    <Input
                      autoComplete="off"
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-username"
                      placeholder="slip@perusahaan.co.id"
                      type="email"
                      {...register("smtp_username", { deps: ["smtp_password"] })}
                    />
                  </Field>
                  <Field error={errors.sender_name?.message} htmlFor="document-mail-sender" label="Nama Pengirim">
                    <Input
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-sender"
                      placeholder="HR Perusahaan"
                      {...register("sender_name")}
                    />
                  </Field>
                  <Field error={errors.smtp_port?.message} htmlFor="document-mail-port" label="Port">
                    <select
                      className="h-10 w-full rounded-sm border-[1.5px] border-transparent bg-surface-muted px-3 text-[14px] text-text-primary outline-none transition-all focus:border-[#4C9AFF] focus:bg-surface focus:shadow-focus"
                      disabled={disabled}
                      id="document-mail-port"
                      {...register("smtp_port", { deps: ["smtp_password"] })}
                    >
                      {portOptions.map((option) => (
                        <option key={option.value} value={option.value}>
                          {option.label}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field htmlFor="document-mail-host" label="Server SMTP">
                    <Input className={inputClass} disabled id="document-mail-host" value="smtp.gmail.com" readOnly />
                  </Field>
                </div>

                <Field error={errors.smtp_password?.message} htmlFor="document-mail-password" label="App Password">
                  <Input
                    autoComplete="new-password"
                    className={inputClass}
                    disabled={disabled || clearingPassword}
                    id="document-mail-password"
                    placeholder={
                      accountChanged
                        ? "Wajib diisi: app password untuk akun/port baru"
                        : accountRemoved
                          ? "Isi alamat Gmail terlebih dahulu"
                          : hasStoredPassword
                            ? "Kosongkan untuk mempertahankan app password saat ini"
                            : "abcd efgh ijkl mnop"
                    }
                    type="password"
                    {...register("smtp_password")}
                  />
                  <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-text-secondary">
                    <span>{passwordNote}</span>
                    <label className="inline-flex items-center gap-2">
                      <input
                        className="h-4 w-4 accent-[var(--module-primary)]"
                        disabled={disabled || !hasStoredPassword || accountChanged}
                        type="checkbox"
                        {...register("clear_smtp_password")}
                      />
                      Hapus password tersimpan
                    </label>
                  </div>
                </Field>

                <div className="rounded-md border border-border bg-surface-muted/40 px-4 py-3 text-sm text-text-secondary">
                  <p className="font-semibold text-text-primary">Sebelum mengisi (Gmail)</p>
                  <ul className="mt-2 list-disc space-y-1 pl-5">
                    <li>Gunakan akun Gmail/Workspace khusus pengirim dokumen, bukan inbox harian seseorang.</li>
                    <li>
                      Aktifkan Verifikasi 2 Langkah di akun tersebut, lalu buat <em>App Password</em> (16
                      karakter) dan tempel di sini.
                    </li>
                    <li>
                      Setiap kali password akun Google diganti, App Password otomatis dicabut. Buat yang baru
                      dan kirim email uji lagi.
                    </li>
                    <li>
                      Mengganti alamat Gmail atau port akan menghapus App Password tersimpan. Mengosongkan
                      alamat Gmail menghapus akun beserta App Password-nya.
                    </li>
                  </ul>
                </div>
              </>
            ) : (
              <>
                <div className="grid gap-4 md:grid-cols-2">
                  <Field
                    error={errors.smtp_forge_base_url?.message}
                    htmlFor="document-mail-forge-url"
                    label="Base URL SMTP Forge"
                    required={enabled}
                  >
                    <Input
                      autoComplete="off"
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-forge-url"
                      placeholder="https://api.smtpforge.com"
                      type="url"
                      {...register("smtp_forge_base_url", { deps: ["smtp_forge_api_key"] })}
                    />
                  </Field>
                  <Field error={errors.sender_name?.message} htmlFor="document-mail-sender" label="Nama Pengirim (Opsional)">
                    <Input
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-sender"
                      placeholder="HR Perusahaan"
                      {...register("sender_name")}
                    />
                  </Field>
                  <Field
                    error={errors.smtp_forge_account_id?.message}
                    htmlFor="document-mail-forge-account"
                    label="SMTP Account ID (Opsional, UUID)"
                  >
                    <Input
                      autoComplete="off"
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-forge-account"
                      placeholder="00000000-0000-0000-0000-000000000000"
                      {...register("smtp_forge_account_id")}
                    />
                  </Field>
                  <Field
                    error={errors.smtp_forge_from_email?.message}
                    htmlFor="document-mail-forge-from"
                    label="From Email (Opsional)"
                  >
                    <Input
                      autoComplete="off"
                      className={inputClass}
                      disabled={disabled}
                      id="document-mail-forge-from"
                      placeholder="slip@perusahaan.co.id"
                      type="email"
                      {...register("smtp_forge_from_email")}
                    />
                  </Field>
                </div>

                <Field
                  error={errors.smtp_forge_api_key?.message}
                  htmlFor="document-mail-forge-key"
                  label="API Key (X-Mailer-Api-Key)"
                >
                  <Input
                    autoComplete="new-password"
                    className={inputClass}
                    disabled={disabled || clearingForgeKey}
                    id="document-mail-forge-key"
                    placeholder={
                      forgeUrlChanged
                        ? "Wajib diisi: API key untuk URL baru"
                        : forgeUrlEmpty
                          ? "Isi Base URL terlebih dahulu"
                          : hasStoredForgeKey
                            ? "Kosongkan untuk mempertahankan API key saat ini"
                            : "Tempel API key dari dashboard SMTP Forge"
                    }
                    type="password"
                    {...register("smtp_forge_api_key")}
                  />
                  <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-text-secondary">
                    <span>{forgeKeyNote}</span>
                    <label className="inline-flex items-center gap-2">
                      <input
                        className="h-4 w-4 accent-[var(--module-primary)]"
                        disabled={disabled || !hasStoredForgeKey || forgeUrlChanged}
                        type="checkbox"
                        {...register("clear_smtp_forge_api_key")}
                      />
                      Hapus API key tersimpan
                    </label>
                  </div>
                </Field>

                <div className="rounded-md border border-border bg-surface-muted/40 px-4 py-3 text-sm text-text-secondary">
                  <p className="font-semibold text-text-primary">Sebelum mengisi (SMTP Forge)</p>
                  <ul className="mt-2 list-disc space-y-1 pl-5">
                    <li>
                      Mode <strong>SMTP Forge</strong> otomatis melewati Mailpit lokal walaupun{" "}
                      <code>APP_ENV=development</code>, sehingga Anda bisa langsung menguji ke server SMTP Forge.
                    </li>
                    <li>
                      <strong>SMTP Account ID</strong> dan <strong>From Email</strong> bersifat opsional; jika
                      kosong, SMTP Forge otomatis memakai akun SMTP aktif yang terhubung ke API key.
                    </li>
                    <li>
                      Pengiriman dokumen dengan lampiran PDF melalui SMTP Forge menggunakan presigned URL dari{" "}
                      <code>STORAGE_DRIVER=s3</code>.
                    </li>
                  </ul>
                </div>
              </>
            )}
          </div>

          <div className="space-y-4 rounded-md border border-border bg-surface-muted/40 p-4">
            <div className="rounded-md border border-border bg-surface px-4 py-3 text-sm">
              <p className="font-semibold text-text-primary">Status saat ini (tersimpan)</p>
              <div className="mt-2 space-y-1 text-text-secondary">
                <p>Email Dokumen aktif: {setting?.enabled ? "Ya" : "Belum"}</p>
                <p>Provider: {setting?.provider === "smtp_forge" ? "SMTP Forge" : "Gmail SMTP"}</p>
                {setting?.provider === "smtp_forge" ? (
                  <>
                    <p>Base URL: {setting.smtp_forge_base_url || "Belum diisi"}</p>
                    <p>API Key: {hasStoredForgeKey ? "Tersimpan" : "Belum ada"}</p>
                    <p>SMTP Account ID: {setting.smtp_forge_account_id || "Otomatis"}</p>
                  </>
                ) : (
                  <>
                    <p>Alamat Gmail: {setting?.smtp_username || "Belum diisi"}</p>
                    <p>App password: {passwordStatus}</p>
                  </>
                )}
                <p>
                  Tujuan pengiriman:{" "}
                  {setting?.provider === "smtp_forge"
                    ? "SMTP Forge Gateway"
                    : setting?.delivery?.mode === "dev_capture"
                      ? `Mailpit lokal (${setting.delivery.capture_addr ?? setting.dev_smtp_addr})`
                      : "Gmail (smtp.gmail.com)"}
                </p>
                {pdfStatus ? (
                  <p data-testid="document-mail-pdf-status">
                    Konversi PDF:{" "}
                    <span
                      className={cn(
                        "font-semibold",
                        pdfStatus.tone === "success"
                          ? "text-success"
                          : pdfStatus.tone === "warning"
                            ? "text-warning"
                            : "text-text-primary",
                      )}
                    >
                      {pdfStatus.text}
                    </span>
                  </p>
                ) : null}
                <p>
                  Siap mengirim:{" "}
                  <span className={cn("font-semibold", setting?.ready ? "text-success" : "text-text-primary")}>
                    {setting?.ready ? "Ya" : "Belum"}
                  </span>
                </p>
              </div>
            </div>

            <div className="space-y-3 rounded-md border border-border bg-surface px-4 py-3">
              <div>
                <p className="text-sm font-semibold text-text-primary">Kirim email uji</p>
                <p className="mt-1 text-xs text-text-secondary">
                  Mengirim ke alamat login Anda memakai nilai formulir saat ini (bisa diuji sebelum disimpan).
                </p>
              </div>
              <Button
                disabled={!canManage || !canTestCurrentForm || testMutation.isPending || settingQuery.isLoading}
                onClick={handleSendTest}
                size="sm"
                type="button"
                variant="outline"
              >
                <FlaskConical className="h-4 w-4" />
                {testMutation.isPending ? "Mengirim..." : "Kirim email uji"}
              </Button>
              {!canTestCurrentForm && !settingQuery.isLoading ? (
                <p className="text-xs text-text-secondary">
                  {provider === "smtp_forge"
                    ? "Isi Base URL dan API key SMTP Forge untuk mengirim email uji."
                    : "Isi alamat Gmail dan app password untuk mengirim email uji."}
                </p>
              ) : null}
              {testResult?.kind === "queued" ? (
                <div className="space-y-1.5 rounded-md border border-border bg-surface-muted/50 p-3 text-xs" role="status">
                  <div className="flex items-center justify-between gap-2 font-medium text-text-primary">
                    <span className="inline-flex items-center gap-1.5">
                      <Loader2 className="h-3.5 w-3.5 animate-spin text-ops" />
                      {testResult.progress?.message || "Mengirim melalui SMTP Forge..."}
                    </span>
                    <span>{testResult.progress?.percentage ?? 0}%</span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-border">
                    <div
                      className="h-full bg-ops transition-all duration-300"
                      style={{ width: `${Math.min(100, Math.max(5, testResult.progress?.percentage ?? 10))}%` }}
                    />
                  </div>
                </div>
              ) : null}
              {testResult?.kind === "sent" ? (
                <p className="flex items-start gap-2 text-sm text-success" role="status">
                  <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" />
                  Email uji terkirim ke {testResult.recipient}.
                </p>
              ) : null}
              {testResult?.kind === "error" ? (
                <p className="flex items-start gap-2 text-sm text-error" role="alert">
                  <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0" />
                  {testResult.message}
                </p>
              ) : null}
            </div>
          </div>
        </div>

        <div className="flex justify-end">
          <Button disabled={disabled || saveMutation.isPending} type="submit">
            <Save className="h-4 w-4" />
            Simpan Email Dokumen
          </Button>
        </div>
      </form>
    </Card>
  );
}

function Field({
  children,
  error,
  htmlFor,
  label,
  required,
}: {
  children: ReactNode;
  error?: string;
  htmlFor: string;
  label: string;
  required?: boolean;
}) {
  return (
    <div className="space-y-1.5">
      <label className="text-[13px] font-[600] text-text-primary" htmlFor={htmlFor}>
        {label}
        {required ? <span className="text-error"> *</span> : null}
      </label>
      {children}
      {error ? <p className="text-xs text-error">{error}</p> : null}
    </div>
  );
}
