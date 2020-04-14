import React, {useState, useRef, useEffect} from 'react';

const ApiKeyPrompt = ({failedKey, setApiKey: submitNewApiKey}) => {
  const [formApiKey, updateFormApiKey] = useState(failedKey);
  const inputEl = useRef(null);
  useEffect(() => inputEl.current.focus());

  return (
    <section>
      <div className="form-group">
        <div className="input mt-3 mb-3">
          <input
            ref={inputEl}
            type="text"
            className="form-control"
            placeholder={
              'Current key: ' +
              (failedKey || process.env.YOLO_APP_PW || 'no key set')
            }
            onChange={(e) => {
              updateFormApiKey(e.target.value);
            }}
          />
        </div>
        <button
          className="btn"
          onClick={() => submitNewApiKey(formApiKey)}
          disabled={!formApiKey}
        >
          Update
        </button>
      </div>
    </section>
  );
};

export default ApiKeyPrompt;
