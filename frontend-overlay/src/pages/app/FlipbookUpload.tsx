import { useState, useRef, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { Upload, FileText, CheckCircle, AlertCircle, Loader2 } from 'lucide-react';
import { flipbookApi, type UsageInfo } from '../../api/flipbook';
import LoadingSpinner from '../../components/LoadingSpinner';

type Phase = 'idle' | 'uploading' | 'converting' | 'ready' | 'error';

const hasFiles = (e: { dataTransfer: DataTransfer | null }) =>
  Array.from(e.dataTransfer?.types ?? []).includes('Files');

export default function FlipbookUpload() {
  const navigate = useNavigate();
  const fileRef = useRef<HTMLInputElement>(null);
  const dropZoneRef = useRef<HTMLDivElement>(null);
  const dragDepth = useRef(0);
  const [dragging, setDragging] = useState(false);
  const [usage, setUsage] = useState<UsageInfo | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [title, setTitle] = useState('');
  const [phase, setPhase] = useState<Phase>('idle');
  const [progress, setProgress] = useState(0);
  const [error, setError] = useState('');
  const [resultSlug, setResultSlug] = useState('');

  useEffect(() => {
    flipbookApi.usage().then((u) => {
      setUsage(u);
      if (u.pagesLimit === 0) {
        navigate('/plan');
      }
    }).catch(() => {});
  }, [navigate]);

  // A file released anywhere but the drop zone (including the zone while it is
  // disabled at the plan limit, or the page mid-upload) would otherwise make
  // the browser open the file and abandon this page.
  useEffect(() => {
    const guard = (e: DragEvent) => {
      if (!hasFiles(e) || dropZoneRef.current?.contains(e.target as Node)) return;
      e.preventDefault();
      if (e.type === 'dragover' && e.dataTransfer) e.dataTransfer.dropEffect = 'none';
      if (e.type === 'drop') {
        dragDepth.current = 0;
        setDragging(false);
      }
    };
    window.addEventListener('dragover', guard);
    window.addEventListener('drop', guard);
    return () => {
      window.removeEventListener('dragover', guard);
      window.removeEventListener('drop', guard);
    };
  }, []);

  const pickFile = (f: File) => {
    const ext = f.name.split('.').pop()?.toLowerCase();
    if (!['pptx', 'ppt', 'pdf'].includes(ext || '')) {
      setError('Only .pptx, .ppt, and .pdf files are supported.');
      return;
    }
    setFile(f);
    setError('');
    setTitle((current) => current || f.name.replace(/\.[^/.]+$/, ''));
  };

  // Entering a child element fires dragleave on the parent, so count depth
  // rather than toggling on every enter/leave.
  const onDragEnter = (e: React.DragEvent) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth.current += 1;
    setDragging(true);
  };

  const onDragOver = (e: React.DragEvent) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  };

  const onDragLeave = (e: React.DragEvent) => {
    if (!hasFiles(e)) return;
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (dragDepth.current === 0) setDragging(false);
  };

  const onDrop = (e: React.DragEvent) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    const files = e.dataTransfer.files;
    if (files.length > 1) {
      setError('Drop one file at a time. Each upload creates one flipbook.');
      return;
    }
    if (files[0]) pickFile(files[0]);
  };

  const handleUpload = async () => {
    if (!file) return;
    setPhase('uploading');
    setError('');
    setProgress(0);

    try {
      const result = await flipbookApi.upload(file, title || undefined, setProgress);
      setPhase('converting');

      // Poll for conversion status
      const poll = setInterval(async () => {
        try {
          const status = await flipbookApi.status(result.id);
          if (status.status === 'ready') {
            clearInterval(poll);
            setResultSlug(status.slug);
            setPhase('ready');
          } else if (status.status === 'error') {
            clearInterval(poll);
            setError(status.error || 'Conversion failed');
            setPhase('error');
          }
        } catch {
          // Keep polling
        }
      }, 2000);
    } catch (err: any) {
      const msg = err?.response?.data?.error || 'Upload failed';
      setError(msg);
      setPhase('error');
    }
  };

  const atLimit = usage && usage.pagesLimit > 0 && usage.pagesUsed >= usage.pagesLimit;

  return (
    <div>
      <h1 className="text-2xl font-bold text-white mb-2">Upload Flipbook</h1>
      <p className="text-dark-400 mb-8">Upload a PowerPoint or PDF to create an interactive flipbook.</p>

      {/* Usage bar */}
      {usage && usage.pagesLimit > 0 && (
        <div className="mb-6 bg-dark-900/50 border border-dark-800 rounded-xl p-4">
          <div className="flex items-center justify-between text-sm mb-2">
            <span className="text-dark-300">Page usage</span>
            <span className="text-dark-400">{usage.pagesUsed} / {usage.pagesLimit} pages</span>
          </div>
          <div className="h-2 bg-dark-800 rounded-full overflow-hidden">
            <div
              className={`h-full rounded-full transition-all ${
                atLimit ? 'bg-red-500' : 'bg-primary-500'
              }`}
              style={{ width: `${Math.min(100, (usage.pagesUsed / usage.pagesLimit) * 100)}%` }}
            />
          </div>
          {atLimit && (
            <p className="text-red-400 text-sm mt-2">
              You've reached your page limit.{' '}
              <a href="/plan" className="underline hover:text-red-300">Upgrade your plan</a> to upload more.
            </p>
          )}
        </div>
      )}

      {phase === 'idle' && (
        <>
          {/* Drop zone */}
          <div
            ref={dropZoneRef}
            className={`border-2 border-dashed rounded-2xl p-12 text-center transition-colors cursor-pointer ${
              dragging
                ? 'border-primary-400 bg-primary-500/10'
                : file ? 'border-primary-500 bg-primary-500/5' : 'border-dark-700 hover:border-dark-600'
            } ${atLimit ? 'opacity-50 pointer-events-none' : ''}`}
            onClick={() => fileRef.current?.click()}
            onDragEnter={onDragEnter}
            onDragOver={onDragOver}
            onDragLeave={onDragLeave}
            onDrop={onDrop}
          >
            <input
              ref={fileRef}
              type="file"
              accept=".pptx,.ppt,.pdf"
              className="hidden"
              onChange={(e) => e.target.files?.[0] && pickFile(e.target.files[0])}
            />
            {file ? (
              <div>
                <FileText className="w-12 h-12 text-primary-400 mx-auto mb-3" />
                <p className="text-white font-medium">{file.name}</p>
                <p className="text-dark-400 text-sm mt-1">
                  {(file.size / (1024 * 1024)).toFixed(1)} MB — Click to change
                </p>
              </div>
            ) : (
              <div>
                <Upload className="w-12 h-12 text-dark-600 mx-auto mb-3" />
                <p className="text-dark-300 font-medium">Drop a file here or click to browse</p>
                <p className="text-dark-500 text-sm mt-1">.pptx, .ppt, or .pdf (max 100MB)</p>
              </div>
            )}
          </div>

          {file && (
            <div className="mt-6 space-y-4">
              <div>
                <label className="block text-sm font-medium text-dark-300 mb-1">Title</label>
                <input
                  type="text"
                  value={title}
                  onChange={(e) => setTitle(e.target.value)}
                  className="w-full bg-dark-900 border border-dark-700 rounded-lg px-3 py-2 text-white focus:border-primary-500 focus:outline-none"
                  placeholder="My Presentation"
                />
              </div>
              <button
                onClick={handleUpload}
                className="w-full py-3 bg-primary-500 text-white rounded-lg font-medium hover:bg-primary-600 transition-colors"
              >
                Upload & Convert
              </button>
            </div>
          )}

          {error && (
            <div className="mt-4 flex items-center gap-2 text-red-400 text-sm">
              <AlertCircle className="w-4 h-4" /> {error}
            </div>
          )}
        </>
      )}

      {phase === 'uploading' && (
        <div className="text-center py-12">
          <Loader2 className="w-12 h-12 text-primary-400 mx-auto mb-4 animate-spin" />
          <p className="text-white font-medium">Uploading...</p>
          <div className="mt-4 max-w-xs mx-auto h-2 bg-dark-800 rounded-full overflow-hidden">
            <div className="h-full bg-primary-500 rounded-full transition-all" style={{ width: `${progress}%` }} />
          </div>
          <p className="text-dark-400 text-sm mt-2">{progress}%</p>
        </div>
      )}

      {phase === 'converting' && (
        <div className="text-center py-12">
          <LoadingSpinner size="lg" className="mb-4" />
          <p className="text-white font-medium">Converting presentation...</p>
          <p className="text-dark-400 text-sm mt-2">This usually takes 30 seconds to a few minutes.</p>
        </div>
      )}

      {phase === 'ready' && (
        <div className="text-center py-12">
          <CheckCircle className="w-16 h-16 text-accent-emerald mx-auto mb-4" />
          <h2 className="text-xl font-bold text-white mb-2">Flipbook ready!</h2>
          <p className="text-dark-400 mb-6">Your flipbook has been created and is live.</p>
          <div className="flex items-center justify-center gap-4">
            <a
              href={`/v/${resultSlug}`}
              target="_blank"
              rel="noopener noreferrer"
              className="px-6 py-2.5 bg-primary-500 text-white rounded-lg font-medium hover:bg-primary-600"
            >
              View Flipbook
            </a>
            <button
              onClick={() => navigate('/flipbooks')}
              className="px-6 py-2.5 bg-dark-800 text-dark-300 border border-dark-700 rounded-lg hover:border-dark-600"
            >
              My Flipbooks
            </button>
          </div>
        </div>
      )}

      {phase === 'error' && (
        <div className="text-center py-12">
          <AlertCircle className="w-16 h-16 text-red-400 mx-auto mb-4" />
          <h2 className="text-xl font-bold text-white mb-2">Something went wrong</h2>
          <p className="text-dark-400 mb-6">{error}</p>
          <button
            onClick={() => { setPhase('idle'); setFile(null); setError(''); }}
            className="px-6 py-2.5 bg-dark-800 text-dark-300 border border-dark-700 rounded-lg hover:border-dark-600"
          >
            Try Again
          </button>
        </div>
      )}
    </div>
  );
}
